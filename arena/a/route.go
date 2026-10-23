package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/arena/keeper"
	"github.com/candyflipline/mnemonic/arena/obj"
	"github.com/candyflipline/mnemonic/arena/route"
)

const headTimeout = 250 * time.Millisecond

// runRoute is `a route`: a stateless router over a read-only store, the
// keeper's /v1/head and /v1/call, and the readers.
func runRoute(args []string) error {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	tenant := fs.String("tenant", "", "tenant")
	storeURI := fs.String("store", "", "object store URI")
	keeperURL := fs.String("keeper", "", "keeper base URL; empty answers committed manifests only")
	readersFlag := fs.String("readers", "", "id=http://host:port,... reader servers")
	listen := fs.String("listen", "127.0.0.1:7200", "HTTP listen address")
	readerTimeout := fs.Duration("reader-timeout", 10*time.Second, "a reader call not answered within this fails over to the part's next reader")
	fs.Parse(args)
	if *tenant == "" || strings.Contains(*tenant, "/") {
		return fmt.Errorf("route: -tenant must be a non-empty name without /")
	}
	readers, err := parseRouteReaders(*readersFlag, *readerTimeout)
	if err != nil {
		return err
	}
	s, err := obj.OpenRead(*storeURI)
	if err != nil {
		return err
	}
	var k route.Keeper
	if *keeperURL != "" {
		k = route.HTTPKeeper(*keeperURL, headTimeout, keeper.DefaultFence.MaxCall)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rt := route.NewRouter(*tenant, s, k, readers)
	go rt.Run(ctx)
	srv := &http.Server{Handler: routeServer{rt: rt}.mux()}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	log.Printf("route %s: listening on %s", *tenant, ln.Addr())
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func parseRouteReaders(flag string, timeout time.Duration) ([]route.Reader, error) {
	var readers []route.Reader
	for _, kv := range strings.Split(flag, ",") {
		if kv == "" {
			continue
		}
		id, u, ok := strings.Cut(kv, "=")
		if !ok || id == "" || id == route.LocalID || u == "" {
			return nil, fmt.Errorf("route: bad -readers entry %q", kv)
		}
		readers = append(readers, route.HTTPReader(id, u, timeout))
	}
	return readers, nil
}

type routeServer struct{ rt *route.Router }

type routeBody struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
	Rung string          `json:"rung"`
}

type routeReply struct {
	Data     json.RawMessage `json:"data"`
	Text     string          `json:"text"`
	Error    string          `json:"error"`
	Gen      keeper.Gen      `json:"gen"`
	Seq      keeper.Seq      `json:"seq"`
	Coverage route.Coverage  `json:"coverage"`
}

var routeRungs = map[string]route.Rung{"whole": route.Whole, "partial": route.Partial}

func (rs routeServer) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("POST /v1/call", rs.serveCall)
	m.HandleFunc("GET /v1/view", rs.serveView)
	return m
}

func (rs routeServer) serveCall(w http.ResponseWriter, r *http.Request) {
	var body routeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rung, ok := routeRungs[body.Rung]
	if !ok {
		http.Error(w, `rung must be "whole" or "partial"`, http.StatusBadRequest)
		return
	}
	ans, err := rs.rt.Call(r.Context(), rung, body.Tool, body.Args)
	var infra route.InfraError
	switch {
	case errors.Is(err, route.ErrStale), errors.Is(err, route.ErrNoKeeper):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case errors.As(err, &infra), err != nil && r.Context().Err() != nil:
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rep := routeReply{Data: ans.Data, Text: ans.Text, Gen: ans.Gen, Seq: ans.Seq, Coverage: ans.Coverage}
	if err != nil {
		rep.Error = err.Error()
	}
	writeRouteJSON(w, rep)
}

func (rs routeServer) serveView(w http.ResponseWriter, r *http.Request) {
	var after route.Cursor
	if s := r.URL.Query().Get("after"); s != "" {
		c, err := route.ParseCursor(s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		after = c
	}
	v, ok, err := rs.rt.View(r.Context(), after)
	switch {
	case errors.Is(err, route.ErrStale):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadGateway)
	case !ok:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeRouteJSON(w, v)
	}
}

func writeRouteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}
