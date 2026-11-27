package gateway

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/EziosWJ/Extension-Host/internal/lifecycle"
	"github.com/EziosWJ/Extension-Host/internal/logtail"
)

func (g *Gateway) logs(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	count := 200
	if values := query["lines"]; len(values) == 1 && err == nil {
		count, err = strconv.Atoi(values[0])
	}
	if err != nil || len(query["lines"]) > 1 || count < 1 || count > 1000 {
		writeCodedError(w, http.StatusBadRequest, "invalid_lines", "lines must be an integer between 1 and 1000")
		return
	}

	id := r.PathValue("id")
	path, ok := g.manager.LogPath(id)
	if !ok {
		g.fail(w, fmt.Errorf("%w: %s", lifecycle.ErrNotFound, id))
		return
	}
	lines, err := readLogLines(path, count)
	if err != nil {
		g.logger.Error("read plugin log", "plugin", id, "error", err)
		writeCodedError(w, http.StatusInternalServerError, "log_read_failed", "could not read plugin log")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"lines": lines})
}

func readLogLines(path string, count int) ([]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start, err := logtail.Start(file, info.Size(), count)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.NewSectionReader(file, start, info.Size()-start))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []string{}, nil
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines, nil
}
