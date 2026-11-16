package coreapi

import (
	"context"
	"minigw/core"
	"path/filepath"
	"strings"
)

func (a *API) adminBundle(ctx context.Context, st *core.Store, r Request) Response {
	if len(r.Body) > MaxBody {
		return a.errResponse(core.ErrTooLarge)
	}
	if r.Method != "POST" || (r.Path != "/export" && r.Path != "/import") {
		return a.errResponse(core.ErrNotFound)
	}
	if r.Path == "/export" {
		var in core.BundleExport
		if err := payload(r, &in); err != nil {
			return a.errResponse(err)
		}
		if !filepath.IsAbs(in.Path) {
			return a.errResponse(core.ErrInvalid{What: "absolute local path required"})
		}
		if err := st.ExportBundle(ctx, in); err != nil {
			return a.errResponse(err)
		}
		return resp(200, map[string]any{"lines": []string{"export " + in.Path}})
	}
	var in core.BundleImport
	if err := payload(r, &in); err != nil {
		return a.errResponse(err)
	}
	if !filepath.IsAbs(in.Path) {
		return a.errResponse(core.ErrInvalid{What: "absolute local path required"})
	}
	report, err := st.ImportBundle(ctx, in)
	if err != nil {
		response := a.errResponse(err)
		// Keep refusal details available to dry-run clients.
		return resp(response.Status, map[string]any{"lines": report.Lines, "error": map[string]string{"message": strings.Join(report.Lines, "\n")}})
	}
	return resp(200, report)
}
