package otlp

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"time"

	colllogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
)

const Harness = "otlp"

// MaxBody is the largest request body, compressed or not, a receiver reads:
// the 64 MiB the OTLP/HTTP spec recommends.
const MaxBody = 64 << 20

// DecodeSlots is how many requests a receiver reads, decodes and converts at
// once. A body of MaxBody is several times that in memory while it is held as
// bytes, JSON, proto and records, so the next request waits for a slot at the
// exporter: it gets 429 with Retry-After, which OTLP exporters retry.
const DecodeSlots = 2

const (
	protobufType = "application/x-protobuf"
	jsonType     = "application/json"
)

type Receiver struct {
	st           *store.Committer
	storeContent atomic.Bool
	slots        chan struct{}
}

func NewReceiver(st *store.Committer) *Receiver {
	return &Receiver{st: st, slots: make(chan struct{}, DecodeSlots)}
}

func (r *Receiver) SetStoreContent(on bool) { r.storeContent.Store(on) }

func (r *Receiver) Traces() http.Handler {
	return r.handler(func() proto.Message { return &colltrace.ExportTraceServiceRequest{} },
		func(m proto.Message, content bool) (*batch, proto.Message) {
			b := fromTraces(m.(*colltrace.ExportTraceServiceRequest), content)
			resp := &colltrace.ExportTraceServiceResponse{}
			if b.rejected > 0 {
				resp.PartialSuccess = &colltrace.ExportTracePartialSuccess{RejectedSpans: b.rejected, ErrorMessage: b.message()}
			}
			return b, resp
		})
}

func (r *Receiver) Logs() http.Handler {
	return r.handler(func() proto.Message { return &colllogs.ExportLogsServiceRequest{} },
		func(m proto.Message, content bool) (*batch, proto.Message) {
			b := fromLogs(m.(*colllogs.ExportLogsServiceRequest), content)
			resp := &colllogs.ExportLogsServiceResponse{}
			if b.rejected > 0 {
				resp.PartialSuccess = &colllogs.ExportLogsPartialSuccess{RejectedLogRecords: b.rejected, ErrorMessage: b.message()}
			}
			return b, resp
		})
}

func (r *Receiver) handler(newReq func() proto.Message, convert func(proto.Message, bool) (*batch, proto.Message)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		media, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if media != protobufType && media != jsonType {
			fail(w, media, http.StatusUnsupportedMediaType, "content type must be application/x-protobuf or application/json")
			return
		}
		select {
		case r.slots <- struct{}{}:
			defer func() { <-r.slots }()
		default:
			w.Header().Set("Retry-After", "1")
			fail(w, media, http.StatusTooManyRequests, "too many requests are being decoded; retry")
			return
		}
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(otlpReadTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			fail(w, media, http.StatusInternalServerError, err.Error())
			return
		}
		body, code, err := readBody(w, req)
		if err != nil {
			fail(w, media, code, err.Error())
			return
		}
		msg := newReq()
		if err := unmarshal(media, body, msg); err != nil {
			fail(w, media, http.StatusBadRequest, "undecodable request: "+err.Error())
			return
		}
		b, resp := convert(msg, r.storeContent.Load())

		author := auth.Author(req.Context())
		for service, recs := range b.byService {
			if len(recs) == 0 {
				continue
			}
			lines, err := encodeLines(author, recs)
			if err != nil {
				fail(w, media, http.StatusInternalServerError, "encode: "+err.Error())
				return
			}
			if _, _, err := r.st.Append(store.Today(author, Harness, service), "", lines); err != nil {
				fail(w, media, http.StatusServiceUnavailable, "store: "+err.Error())
				return
			}
		}
		out, err := marshal(media, resp)
		if err != nil {
			fail(w, media, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", media)
		w.Write(out)
	})
}

var otlpReadTimeout = 30 * time.Second

var errReadTimeout = errors.New("request body did not arrive in time")

func readBody(w http.ResponseWriter, req *http.Request) ([]byte, int, error) {
	var src io.Reader = http.MaxBytesReader(w, req.Body, MaxBody)
	switch enc := req.Header.Get("Content-Encoding"); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, http.StatusRequestTimeout, errReadTimeout
			}
			return nil, http.StatusBadRequest, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		src = zr
	default:
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("content encoding %q is not gzip", enc)
	}
	body, err := io.ReadAll(io.LimitReader(src, MaxBody+1))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig) || len(body) > MaxBody:
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("request body above %d bytes", MaxBody)
	case errors.Is(err, os.ErrDeadlineExceeded):
		return nil, http.StatusRequestTimeout, errReadTimeout
	case err != nil:
		return nil, http.StatusBadRequest, fmt.Errorf("read body: %w", err)
	}
	return body, 0, nil
}

func unmarshal(media string, body []byte, msg proto.Message) error {
	if media == protobufType {
		return proto.Unmarshal(body, msg)
	}
	b, err := idsToBase64(body)
	if err != nil {
		return err
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, msg)
}

func marshal(media string, msg proto.Message) ([]byte, error) {
	if media == protobufType {
		return proto.Marshal(msg)
	}
	return protojson.Marshal(msg)
}

func fail(w http.ResponseWriter, media string, code int, msg string) {
	if media != jsonType {
		media = protobufType
	}
	out, _ := marshal(media, &status.Status{Code: int32(code), Message: msg})
	w.Header().Set("Content-Type", media)
	w.WriteHeader(code)
	w.Write(out)
}

var idFields = []string{"traceId", "trace_id", "spanId", "span_id", "parentSpanId", "parent_span_id"}

// idsToBase64 rewrites the trace and span ids of an OTLP/JSON body from the
// hex the OTLP spec writes them in to the base64 protojson reads for bytes.
func idsToBase64(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if err := walkIDs(doc); err != nil {
		return nil, err
	}
	return json.Marshal(doc)
}

func walkIDs(v any) error {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && slices.Contains(idFields, k) {
				raw, err := hex.DecodeString(s)
				if err != nil {
					return fmt.Errorf("%s %q is not hex", k, s)
				}
				x[k] = base64.StdEncoding.EncodeToString(raw)
				continue
			}
			if err := walkIDs(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := walkIDs(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func encodeLines(author string, recs []Record) ([]byte, error) {
	var buf bytes.Buffer
	for i := range recs {
		recs[i].Author = author
		b, err := json.Marshal(&recs[i])
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}
