package obj

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFileStore(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	s, err := Open("file://" + t.TempDir() + "/store?serve=http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, s) }()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; ; i++ {
		if resp, err := http.Get("http://" + addr + "/stats"); err == nil {
			resp.Body.Close()
			break
		}
		if i > 200 {
			t.Fatal("serve never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	put := func(k string, body string) error {
		return s.Put(ctx, Key(k), strings.NewReader(body), int64(len(body)))
	}
	list := func(prefix string) string {
		es, err := s.List(ctx, Key(prefix))
		if err != nil {
			return err.Error()
		}
		var parts []string
		for _, e := range es {
			parts = append(parts, fmt.Sprintf("%s:%d", e.Key, e.Size))
		}
		return strings.Join(parts, " ")
	}
	get := func(k string, ttl time.Duration, rng string, tamper bool) (int, string, string) {
		u, err := s.PresignGetUntil(Key(k), time.Now().Add(ttl))
		if err != nil {
			t.Fatal(err)
		}
		if tamper {
			u = u[:len(u)-1] + map[bool]string{true: "0", false: "1"}[u[len(u)-1] == '1']
		}
		req, _ := http.NewRequest("GET", u, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header.Get("Content-Range")
	}

	rows := []struct {
		name string
		got  func() any
		want any
	}{
		{"put b/2", func() any { return put("b/2", "bb") }, nil},
		{"put a/1", func() any { return put("a/1", "a") }, nil},
		{"put b/10", func() any { return put("b/10", "ten") }, nil},
		{"list all sorted", func() any { return list("") }, "a/1:1 b/10:3 b/2:2"},
		{"list prefix b/", func() any { return list("b/") }, "b/10:3 b/2:2"},
		{"list prefix b/1", func() any { return list("b/1") }, "b/10:3"},
		{"exclusive first", func() any { return s.PutExclusive(ctx, "g/1", strings.NewReader("one"), 3) }, nil},
		{"exclusive second", func() any { return errors.Is(s.PutExclusive(ctx, "g/1", strings.NewReader("two"), 3), ErrExists) }, true},
		{"exclusive kept first bytes", func() any { _, b, _ := get("g/1", time.Minute, "", false); return b }, "one"},
		{"delete present and missing", func() any { return s.Delete(ctx, []Key{"a/1", "nope"}) }, nil},
		{"list after delete", func() any { return list("") }, "b/10:3 b/2:2 g/1:3"},
		{"put digits", func() any { return put("d", "0123456789") }, nil},
		{"get whole", func() any { c, b, _ := get("d", time.Minute, "", false); return fmt.Sprintf("%d %s", c, b) }, "200 0123456789"},
		{"get range", func() any {
			c, b, r := get("d", time.Minute, "bytes=2-5", false)
			return fmt.Sprintf("%d %s %s", c, b, r)
		}, "206 2345 bytes 2-5/10"},
		{"tampered sig", func() any { c, _, _ := get("d", time.Minute, "", true); return c }, 403},
		{"expired", func() any { c, _, _ := get("d", -time.Minute, "", false); return c }, 403},
		{"missing object", func() any { c, _, _ := get("nope", time.Minute, "", false); return c }, 404},
		{"dotdot key", func() any { return put("a/../b", "x") != nil }, true},
		{"empty key", func() any { return put("", "x") != nil }, true},
		{"stats", func() any {
			resp, err := http.Get("http://" + addr + "/stats")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			var st map[string]int64
			json.NewDecoder(resp.Body).Decode(&st)
			return fmt.Sprint(st["requests"], st["bytes"])
		}, "3 17"},
	}
	for _, r := range rows {
		if got := r.got(); !reflect.DeepEqual(got, r.want) {
			t.Errorf("%s: got %v, want %v", r.name, got, r.want)
		}
	}
}
