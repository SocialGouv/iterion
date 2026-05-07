[← Documentation index](README.md) · [← Iterion](../README.md)

# Visual Editor (web)

Iterion includes a browser-based visual workflow editor built with React and XYFlow. Served by your local `iterion` binary — no installation beyond the CLI.

```bash
iterion editor                     # Launch on default port (4891), opens browser
iterion editor --port 8080         # Custom port
iterion editor --dir ./workflows   # Custom working directory
iterion editor --no-browser        # Don't auto-open browser
```

## What you get

- **Canvas** — Drag-and-drop node graph with auto-layout, zoom, search, and keyboard shortcuts
- **Node library** — Drag pre-built node types (agent, judge, router, join, human, tool) onto the canvas
- **Property editor** — Edit node properties, schemas, prompts, and edge conditions in a side panel
- **Source view** — Split-pane view showing the raw `.iter` source alongside the visual graph
- **Live diagnostics** — Real-time validation errors and warnings as you edit (codes C001–C043)
- **File watching** — Detects external file changes via WebSocket and syncs automatically
- **Undo/redo** — Full edit history
- **Run console** — Launch a workflow from the editor and watch events stream live

This mode is the simplest way to design and iterate locally. If you want a packaged native window instead (no browser, OS-keychain credentials, auto-update), see the [Desktop App](desktop.md).
