(() => {
  const revision = document.currentScript.dataset.revision;

  async function checkForChanges() {
    try {
      const response = await fetch("/__godesk_dev/revision", { cache: "no-store" });
      if (response.ok && (await response.text()) !== revision) {
        window.location.reload();
        return;
      }
    } catch {
      // The development host may be restarting.
    }
    setTimeout(checkForChanges, 400);
  }

  setTimeout(checkForChanges, 400);
})();
