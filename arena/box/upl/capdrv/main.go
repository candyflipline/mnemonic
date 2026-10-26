// Command capdrv is the box tool for the live-session cap proof. Against a
// plaintext push listener it opens N distinct machines under one token, one
// connection each, waits for each Welcome, sends one synthetic Append and waits
// for its Ack, then prints one row per client with the status code the server
// gave it. It then opens the first acked machine again (a takeover at the
// cap) and reports the status the displaced stream ended with. Tooling only;
// counts and codes, never content.
//
//	capdrv -server host:port -token T -cap N [-prefix cap]
//
// Exit 0 when exactly one of the N+1 clients was ResourceExhausted, the other
// N were acked, the takeover was acked and the displaced stream ended Aborted.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/candyflipline/mnemonic/internal/wire/pushpb"
)

type client struct {
	machine string
	conn    *grpc.ClientConn
	stream  pushpb.Push_SyncClient
	cancel  context.CancelFunc
	code    codes.Code
	acked   bool
	done    chan codes.Code
}

func open(server, token, machine string) (*client, error) {
	conn, err := grpc.NewClient("passthrough:///"+server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token))
	stream, err := pushpb.NewPushClient(conn).Sync(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	c := &client{machine: machine, conn: conn, stream: stream, cancel: cancel, code: codes.OK, done: make(chan codes.Code, 1)}
	if err := stream.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Hello{Hello: &pushpb.Hello{Machine: machine}}}); err != nil {
		c.code = status.Code(err)
	}
	return c, nil
}

func (c *client) welcome() {
	for c.code == codes.OK {
		msg, err := c.stream.Recv()
		if err != nil {
			c.code = status.Code(err)
			return
		}
		if w := msg.GetWelcome(); w != nil && !w.More {
			return
		}
	}
}

func (c *client) appendAck() {
	if c.code != codes.OK {
		return
	}
	path := "/capdrv/" + c.machine + ".jsonl"
	body := []byte(`{"t":"capdrv"}` + "\n")
	err := c.stream.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Append{Append: &pushpb.Append{
		Seq: 1, Harness: "claude-code", SrcId: wire.SrcID(path), Offset: 0, HeadHash: "capdrv",
		SourceName: "claude", Path: path, RawEnd: int64(len(body)), Body: body,
	}}})
	if err != nil {
		c.code = status.Code(err)
		return
	}
	msg, err := c.stream.Recv()
	if err != nil {
		c.code = status.Code(err)
		return
	}
	c.acked = msg.GetAck() != nil
}

func (c *client) watch() {
	go func() {
		_, err := c.stream.Recv()
		c.done <- status.Code(err)
	}()
}

func (c *client) close() {
	c.cancel()
	c.conn.Close()
}

func ack(c *client) string {
	if c.acked {
		return "ack"
	}
	return "-"
}

func main() {
	server := flag.String("server", "", "host:port of the plaintext push listener")
	token := flag.String("token", "", "push-scope bearer token")
	capN := flag.Int("cap", 20, "the server's live-session cap")
	prefix := flag.String("prefix", "cap", "machine name prefix")
	flag.Parse()
	n := *capN + 1
	cs := make([]*client, n)
	var wg sync.WaitGroup
	for i := range cs {
		c, err := open(*server, *token, fmt.Sprintf("%s%02d", *prefix, i))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cs[i] = c
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.welcome()
			c.appendAck()
		}()
		time.Sleep(200 * time.Millisecond)
	}
	wg.Wait()
	exhausted, acked := 0, 0
	for i, c := range cs {
		fmt.Printf("client %d %s %s %s\n", i, c.machine, c.code, ack(c))
		if c.code == codes.ResourceExhausted {
			exhausted++
		}
		if c.acked {
			acked++
			c.watch()
		}
	}
	var first *client
	for _, c := range cs {
		if c.acked {
			first = c
			break
		}
	}
	ok := exhausted == 1 && acked == n-1
	if first != nil {
		t, err := open(*server, *token, first.machine)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		t.welcome()
		t.appendAck()
		var old codes.Code
		select {
		case old = <-first.done:
		case <-time.After(10 * time.Second):
			old = codes.DeadlineExceeded
		}
		fmt.Printf("takeover %s %s %s old=%s\n", t.machine, t.code, ack(t), old)
		ok = ok && t.code == codes.OK && old == codes.Aborted
		t.close()
	}
	for _, c := range cs {
		c.close()
	}
	fmt.Printf("summary cap=%d clients=%d acked=%d resource_exhausted=%d pass=%v\n", *capN, n, acked, exhausted, ok)
	if !ok {
		os.Exit(1)
	}
}
