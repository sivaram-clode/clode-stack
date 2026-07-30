package composio

// tools.go holds the "workable capability": each tool slug maps to a small
// Postgres read or write, scoped to the calling connect-ref. The point is that a
// write is really persisted and a later read returns it — agents can treat these
// like real tools. Semantics are deliberately minimal, not faithful to Google's
// real APIs.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// defaultSpreadsheet is the single sheet a connect-ref writes to when none is
// named — enough to make append→read round-trip without modeling sheet ids.
const defaultSpreadsheet = "sheet_default"

// executeTool runs one tool for a connect-ref and returns the JSON-able `data`
// payload. A returned error becomes {successful:false,error:…} — never a panic,
// so a bad argument is a predictable failure, not a mock bug to chase.
func executeTool(ctx context.Context, s *store, accountRef, slug string, args map[string]any) (any, error) {
	switch slug {

	// ── gmail ──────────────────────────────────────────────────────────────
	case "GMAIL_SEND_EMAIL", "GMAIL_CREATE_DRAFT":
		to := argStr(args, "to")
		if to == "" {
			return nil, fmt.Errorf("'to' is required")
		}
		label, idKey := "SENT", "message_id"
		if slug == "GMAIL_CREATE_DRAFT" {
			label, idKey = "DRAFT", "draft_id"
		}
		id := newID("msg")
		rec := map[string]any{"id": id, "to": to, "subject": argStr(args, "subject"), "body": argStr(args, "body"), "label": label}
		if err := s.insertResource(ctx, id, accountRef, "gmail", "message", rec); err != nil {
			return nil, err
		}
		return map[string]any{idKey: id, "label": label}, nil

	case "GMAIL_FETCH_EMAILS":
		msgs, err := s.listRaw(ctx, accountRef, "gmail", "message")
		if err != nil {
			return nil, err
		}
		return map[string]any{"messages": msgs, "count": len(msgs)}, nil

	case "GMAIL_GET_MESSAGE":
		return s.getOne(ctx, accountRef, argStr(args, "message_id"), "message")

	// ── googlesheets ───────────────────────────────────────────────────────
	case "GOOGLESHEETS_APPEND_VALUES":
		rows := argValues(args)
		if len(rows) == 0 {
			return nil, fmt.Errorf("'values' must be a non-empty array of rows")
		}
		for _, row := range rows {
			id := newID("row")
			if err := s.insertResource(ctx, id, accountRef, "googlesheets", "sheet_row", map[string]any{"values": row}); err != nil {
				return nil, err
			}
		}
		return map[string]any{"spreadsheetId": spreadsheetID(args), "updates": map[string]any{"updatedRows": len(rows)}}, nil

	case "GOOGLESHEETS_UPDATE_VALUES":
		rows := argValues(args)
		if err := s.deleteResources(ctx, accountRef, "googlesheets", "sheet_row"); err != nil {
			return nil, err
		}
		cells := 0
		for _, row := range rows {
			id := newID("row")
			if err := s.insertResource(ctx, id, accountRef, "googlesheets", "sheet_row", map[string]any{"values": row}); err != nil {
				return nil, err
			}
			cells += len(row)
		}
		return map[string]any{"spreadsheetId": spreadsheetID(args), "updatedRows": len(rows), "updatedCells": cells}, nil

	case "GOOGLESHEETS_BATCH_GET":
		recs, err := s.listRaw(ctx, accountRef, "googlesheets", "sheet_row")
		if err != nil {
			return nil, err
		}
		values := make([]any, 0, len(recs))
		for _, r := range recs {
			var row struct {
				Values any `json:"values"`
			}
			_ = json.Unmarshal(r, &row)
			values = append(values, row.Values)
		}
		return map[string]any{"valueRanges": []any{map[string]any{"range": "Sheet1", "values": values}}}, nil

	case "GOOGLESHEETS_GET_SPREADSHEET":
		recs, err := s.listRaw(ctx, accountRef, "googlesheets", "sheet_row")
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"spreadsheetId": spreadsheetID(args),
			"sheets":        []any{map[string]any{"title": "Sheet1", "rowCount": len(recs)}},
		}, nil

	// ── googledrive ────────────────────────────────────────────────────────
	case "GOOGLEDRIVE_UPLOAD_FILE":
		name := argStr(args, "name")
		if name == "" {
			return nil, fmt.Errorf("'name' is required")
		}
		id := newID("file")
		rec := map[string]any{"id": id, "name": name, "mimeType": orDefault(argStr(args, "mimeType"), "text/plain"), "content": argStr(args, "content"), "isFolder": false}
		if err := s.insertResource(ctx, id, accountRef, "googledrive", "file", rec); err != nil {
			return nil, err
		}
		return map[string]any{"file_id": id, "name": name}, nil

	case "GOOGLEDRIVE_CREATE_FOLDER":
		name := argStr(args, "name")
		if name == "" {
			return nil, fmt.Errorf("'name' is required")
		}
		id := newID("fldr")
		rec := map[string]any{"id": id, "name": name, "mimeType": "application/vnd.google-apps.folder", "isFolder": true}
		if err := s.insertResource(ctx, id, accountRef, "googledrive", "file", rec); err != nil {
			return nil, err
		}
		return map[string]any{"folder_id": id, "name": name}, nil

	case "GOOGLEDRIVE_LIST_FILES":
		recs, err := s.listRaw(ctx, accountRef, "googledrive", "file")
		if err != nil {
			return nil, err
		}
		files := make([]any, 0, len(recs))
		for _, r := range recs {
			var f map[string]any
			_ = json.Unmarshal(r, &f)
			delete(f, "content") // listings omit body, like the real Drive list
			files = append(files, f)
		}
		return map[string]any{"files": files, "count": len(files)}, nil

	case "GOOGLEDRIVE_DOWNLOAD_FILE":
		return s.getOne(ctx, accountRef, argStr(args, "file_id"), "file")

	// ── googlecalendar ─────────────────────────────────────────────────────
	case "GOOGLECALENDAR_CREATE_EVENT":
		summary := argStr(args, "summary")
		if summary == "" {
			return nil, fmt.Errorf("'summary' is required")
		}
		id := newID("evt")
		rec := map[string]any{"id": id, "summary": summary, "start": argStr(args, "start"), "end": argStr(args, "end")}
		if err := s.insertResource(ctx, id, accountRef, "googlecalendar", "event", rec); err != nil {
			return nil, err
		}
		return map[string]any{"event_id": id, "summary": summary}, nil

	case "GOOGLECALENDAR_LIST_EVENTS":
		events, err := s.listRaw(ctx, accountRef, "googlecalendar", "event")
		if err != nil {
			return nil, err
		}
		return map[string]any{"events": events, "count": len(events)}, nil

	// ── notion ─────────────────────────────────────────────────────────────
	case "NOTION_CREATE_PAGE":
		title := argStr(args, "title")
		if title == "" {
			return nil, fmt.Errorf("'title' is required")
		}
		id := newID("page")
		rec := map[string]any{"id": id, "title": title, "content": argStr(args, "content")}
		if err := s.insertResource(ctx, id, accountRef, "notion", "page", rec); err != nil {
			return nil, err
		}
		return map[string]any{"page_id": id, "url": "https://mock.notion.local/" + id}, nil

	case "NOTION_GET_PAGE":
		return s.getOne(ctx, accountRef, argStr(args, "page_id"), "page")

	case "NOTION_SEARCH":
		recs, err := s.listRaw(ctx, accountRef, "notion", "page")
		if err != nil {
			return nil, err
		}
		q := strings.ToLower(argStr(args, "query"))
		results := make([]any, 0, len(recs))
		for _, r := range recs {
			var p struct {
				Title string `json:"title"`
			}
			_ = json.Unmarshal(r, &p)
			if q == "" || strings.Contains(strings.ToLower(p.Title), q) {
				results = append(results, r)
			}
		}
		return map[string]any{"results": results, "count": len(results)}, nil

	// ── slack ──────────────────────────────────────────────────────────────
	case "SLACK_LIST_CHANNELS":
		chans := make([]any, 0, len(slackChannels))
		for i, name := range slackChannels {
			chans = append(chans, map[string]any{"id": fmt.Sprintf("C%03d", i+1), "name": name})
		}
		return map[string]any{"channels": chans}, nil

	case "SLACK_SEND_MESSAGE":
		channel := argStr(args, "channel")
		text := argStr(args, "text")
		if channel == "" || text == "" {
			return nil, fmt.Errorf("'channel' and 'text' are required")
		}
		id := newID("msg")
		rec := map[string]any{"id": id, "channel": channel, "text": text}
		if err := s.insertResource(ctx, id, accountRef, "slack", "slack_msg", rec); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "ts": id, "channel": channel}, nil
	}

	return nil, fmt.Errorf("tool %q is not implemented by the mock", slug)
}

// listRaw returns the stored JSON objects for a (connect-ref, toolkit, kind), in
// insertion order, ready to embed directly in a response.
func (s *store) listRaw(ctx context.Context, accountRef, toolkit, kind string) ([]json.RawMessage, error) {
	rows, err := s.listResources(ctx, accountRef, toolkit, kind)
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Data)
	}
	return out, nil
}

// getOne fetches a single stored object by id, erroring if absent.
func (s *store) getOne(ctx context.Context, accountRef, id, kind string) (any, error) {
	if id == "" {
		return nil, fmt.Errorf("an id is required")
	}
	r, ok, err := s.getResource(ctx, accountRef, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s %q not found", kind, id)
	}
	return r.Data, nil
}

// ── argument helpers ──────────────────────────────────────────────────────────

func argStr(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// argValues coerces the `values` argument into rows. It accepts a 2-D array
// ([[..],[..]]); a bare 1-D array is treated as a single row.
func argValues(args map[string]any) [][]any {
	raw, ok := args["values"].([]any)
	if !ok {
		return nil
	}
	out := make([][]any, 0, len(raw))
	for _, r := range raw {
		if row, ok := r.([]any); ok {
			out = append(out, row)
		} else {
			out = append(out, []any{r})
		}
	}
	return out
}

func spreadsheetID(args map[string]any) string {
	return orDefault(argStr(args, "spreadsheet_id"), defaultSpreadsheet)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// newID mints a short, prefixed identifier (e.g. evt_1a2b3c4d).
func newID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}
