#!/bin/sh
# A file-backed PostgreSQL client fixture. It never opens a database connection.
set -eu
: "${PG_BACKUP_TEST_CATALOG:?}"
test "${PGHOST:-}" = fixture.invalid
test "${PGPORT:-}" = 5432
test "${PGUSER:-}" = fixture
test "${PGPASSWORD:-}" = fixture-only
test -z "${PGSERVICE+x}${PGHOSTADDR+x}"

case "${0##*/}" in
  pg_dump)
    test "$#" = 5
    test "$1 $2 $3" = '--format=custom --no-password --file'
    test "$5" = "--dbname=dbname='original'"
    test "$PGDATABASE" = original
    printf 'fixture archive\n' > "$4"
    if [ -f "$PG_BACKUP_TEST_CATALOG/fail-dump" ]; then
      printf 'fixture dump failed\n' >&2
      exit 1
    fi
    exit
    ;;
  pg_restore)
    test "$1 $2 $3 $4" = '--no-password --exit-on-error --single-transaction --dbname'
    test "$5" = "dbname='$PGDATABASE'"
    test "$6 $7" = '--no-owner --no-acl'
    test -f "$PG_BACKUP_TEST_CATALOG/$PGDATABASE"
    test "$(cat "$8")" = 'fixture archive'
    exit
    ;;
  psql) ;;
  *) exit 1 ;;
esac

output=
sql=
database=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output=$2; shift ;;
    --command) sql=$2; shift ;;
    --dbname=*) database=${1#--dbname=} ;;
  esac
  shift
done
test "$database" = "dbname='$PGDATABASE'"
: > "$output"
cd "$PG_BACKUP_TEST_CATALOG"
case "$sql" in
  *'FROM pg_namespace'*)
    test "$PGDATABASE" = original
    printf '[{"name":"public"}]\n' > "$output"
    ;;
  *'has_database_privilege'*)
    test "$PGDATABASE" = control
    printf '[{"name":"original"}]\n' > "$output"
    ;;
  *'oid::text AS oid'*)
    test "$PGDATABASE" = control
    separator=
    {
      printf '['
      for name in *; do
        case "$sql" in
          *"E'$name'"*)
            printf '%s{"name":"%s","oid":"%s","template":false,"owned":true}' "$separator" "$name" "$(cat "$name")"
            separator=,
            ;;
        esac
      done
      printf ']\n'
    } > "$output"
    ;;
  'CREATE DATABASE '*)
    test "$PGDATABASE" = control
    name=$(printf '%s\n' "$sql" | sed -n 's/^CREATE DATABASE "\([^"]*\)" TEMPLATE template0$/\1/p')
    test -n "$name"
    oid=20
    if [ -f published ]; then oid=21; fi
    printf '%s\n' "$oid" > "$name"
    ;;
  'BEGIN;'*'COMMIT;')
    test "$PGDATABASE" = control
    renames=$(printf '%s' "$sql" | tr ';' '\n' | sed -n 's/^ALTER DATABASE "\([^"]*\)" RENAME TO "\([^"]*\)"$/\1 \2/p')
    test -n "$renames"
    printf '%s\n' "$renames" | while read -r from to; do
      test -f "$from"
      test ! -e "$to"
      mv -- "$from" "$to"
    done
    ;;
  'DROP DATABASE '*)
    test "$PGDATABASE" = control
    name=$(printf '%s\n' "$sql" | sed -n 's/^DROP DATABASE "\([^"]*\)"$/\1/p')
    test -n "$name"
    rm -- "$name"
    printf 'fixture connection lost after dropping database\n' >&2
    exit 1
    ;;
  *)
    printf 'unexpected fixture SQL: %s\n' "$sql" >&2
    exit 1
    ;;
esac
