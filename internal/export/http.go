package export

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/index"
)

// SkippedTrailer is the dataset response's trailer: "<session id>: <error>"
// per session that failed validation.
const SkippedTrailer = "Mnemonic-Skipped"

type request struct {
	kind, session  string
	annotation     AnnotationFilter
	perTask        int
	includePrivate bool
}

// Handler serves GET /v1/export/{kind} from ix's snapshot, pinned for the
// whole response, behind a Read token (docs/spec/export.md).
func Handler(ix *index.Index, ctl *index.Controls) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := parseRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		db, release := ix.Snapshot()
		defer release()
		sessions, err := scope(index.WithControls(r.Context(), ctl), db, req.includePrivate)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		hidden := ctl.ExportHidden(true)
		if req.kind == "dataset" {
			serveDataset(w, db, req, hidden, sessions)
			return
		}
		if !sessions.Opened(req.session) {
			http.Error(w, notFound(req.session).Error(), http.StatusNotFound)
			return
		}
		body, err := session(db, req.kind, req.session, hidden)
		switch {
		case errors.Is(err, ErrSessionNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, ErrInvalidATIF):
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		}
	})
}

// scope is what the caller may export: the read tools' SessionFilter, or with
// includePrivate every session, which takes an admin token whose tenant is
// every author.
func scope(ctx context.Context, db *index.DB, includePrivate bool) (index.SessionFilter, error) {
	if !includePrivate {
		return index.Sessions(ctx, db)
	}
	id, _ := auth.From(ctx)
	members := id.Members()
	if !id.Can(auth.Admin) || !members.Except || len(members.Names) > 0 {
		return index.SessionFilter{}, errors.New("export: include_private needs an admin token over every author")
	}
	return index.SessionFilter{}, nil
}

func session(db *index.DB, kind, sessionID string, hidden map[string]bool) ([]byte, error) {
	if kind == "atif" {
		body, err := MarshalATIF(db, sessionID, hidden)
		if err != nil {
			return nil, err
		}
		return indent(body), nil
	}
	doc, err := OTLP(db, sessionID, hidden)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(doc, "", "  ")
}

func serveDataset(w http.ResponseWriter, db *index.DB, req request, hidden map[string]bool, sessions index.SessionFilter) {
	w.Header().Set("Trailer", SkippedTrailer)
	w.Header().Set("Content-Type", "application/x-ndjson")
	wrote := false
	skipped, err := Dataset(db, req.annotation, req.perTask, hidden, sessions.Listed, func(row DatasetRow) error {
		line, err := json.Marshal(row)
		if err != nil {
			return nil
		}
		wrote = true
		_, err = w.Write(append(line, '\n'))
		return err
	})
	for _, s := range skipped {
		w.Header().Add(SkippedTrailer, s.SessionID+": "+s.Err.Error())
	}
	if err != nil && !wrote {
		code := http.StatusInternalServerError
		if errors.Is(err, ErrEveryFailed) {
			code = http.StatusUnprocessableEntity
		}
		http.Error(w, err.Error(), code)
	}
}

func parseRequest(r *http.Request) (request, error) {
	q := r.URL.Query()
	req := request{kind: r.PathValue("kind"), perTask: 3}
	var err error
	if v := q.Get("include_private"); v != "" {
		if req.includePrivate, err = strconv.ParseBool(v); err != nil {
			return req, fmt.Errorf("export: bad include_private %q", v)
		}
	}
	switch req.kind {
	case "atif", "otlp":
		if req.session, err = rawSessionID(q.Get("session")); err != nil {
			return req, err
		}
		if req.session == "" {
			return req, errors.New("export: session is required")
		}
	case "dataset":
		if req.annotation, err = parseAnnotation(q.Get("annotation")); err != nil {
			return req, err
		}
		if v := q.Get("per_task"); v != "" {
			if req.perTask, err = strconv.Atoi(v); err != nil || req.perTask < 1 {
				return req, fmt.Errorf("export: per_task must be a whole number of at least 1, not %q", v)
			}
		}
	default:
		return req, fmt.Errorf("export: unknown kind %q; atif, otlp or dataset", req.kind)
	}
	return req, nil
}

// indent makes a single-session document readable on a terminal.
func indent(body []byte) []byte {
	var doc any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return body
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return body
	}
	return out
}

// parseAnnotation splits annotation's name[=value].
func parseAnnotation(s string) (AnnotationFilter, error) {
	name, value, hasValue := strings.Cut(s, "=")
	if name == "" {
		return AnnotationFilter{}, errors.New("export: annotation needs a name")
	}
	return AnnotationFilter{Name: name, Value: value, HasValue: hasValue}, nil
}

// rawSessionID accepts the raw session id and the MCP-facing
// session:<base64> form (59796bc:internal/query/common.go:166).
func rawSessionID(s string) (string, error) {
	raw, ok := strings.CutPrefix(s, "session:")
	if !ok {
		return s, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("export: bad session id %q: %w", s, err)
	}
	return string(decoded), nil
}
