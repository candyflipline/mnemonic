package export

import (
	"context"
	"encoding/base64"
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
		w.Header().Set("Content-Type", "application/json")
		out := &startedBody{w: w}
		if req.kind == "atif" {
			err = WriteATIF(out, db, req.session, hidden)
		} else {
			err = WriteOTLP(out, db, req.session, hidden)
		}
		switch {
		case err == nil:
		case errors.Is(err, ErrSessionNotFound):
			out.fail(err, http.StatusNotFound)
		case errors.Is(err, ErrInvalidATIF):
			out.fail(err, http.StatusUnprocessableEntity)
		default:
			out.fail(err, http.StatusInternalServerError)
		}
	})
}

type startedBody struct {
	w http.ResponseWriter
	n int64
}

func (b *startedBody) Write(p []byte) (int, error) {
	n, err := b.w.Write(p)
	b.n += int64(n)
	return n, err
}

func (b *startedBody) fail(err error, status int) {
	if b.n > 0 {
		panic(http.ErrAbortHandler)
	}
	http.Error(b.w, err.Error(), status)
}

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

func serveDataset(w http.ResponseWriter, db *index.DB, req request, hidden map[string]bool, sessions index.SessionFilter) {
	w.Header().Set("Trailer", SkippedTrailer)
	w.Header().Set("Content-Type", "application/x-ndjson")
	out := &startedBody{w: w}
	skipped, err := Dataset(out, db, req.annotation, req.perTask, hidden, sessions.Listed)
	for _, s := range skipped {
		w.Header().Add(SkippedTrailer, s.SessionID+": "+s.Err.Error())
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrEveryFailed):
		out.fail(err, http.StatusUnprocessableEntity)
	default:
		out.fail(err, http.StatusInternalServerError)
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

func parseAnnotation(s string) (AnnotationFilter, error) {
	name, value, hasValue := strings.Cut(s, "=")
	if name == "" {
		return AnnotationFilter{}, errors.New("export: annotation needs a name")
	}
	return AnnotationFilter{Name: name, Value: value, HasValue: hasValue}, nil
}

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
