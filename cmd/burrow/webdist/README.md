# Staged PWA build

This folder is filled by `scripts/build.ps1`, which runs `npm run build` in `web/`
and copies `web/dist/*` here so `cmd/burrow/run.go` can embed it.
Only this README is committed.
