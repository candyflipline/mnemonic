// Command crashdrv is crash.sh's tooling over the push stream; it is never
// shipped. Subcommands:
//
//	proxy    forwards one client's Sync to serve, logs every Append, Welcome
//	         page and Ack with its receive time, and can run serially (one
//	         append in flight) or hold the stream at a kill point.
//	welcome  prints serve's Welcome as "src_id<TAB>size" lines.
//	killat   SIGKILLs a pid while one of its threads is held by strace's
//	         delay_enter at a chosen syscall, counted from the strace log.
//	ackcheck checks a serial run: each Ack arrives after the syncfs that
//	         follows its manifest rename returned.
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/qnet"
	"github.com/candyflipline/mnemonic/internal/wire/pushpb"
)

const maxMsg = 64 << 20

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("usage: crashdrv proxy|welcome|killat|ackcheck [flags]"))
	}
	cmds := map[string]func([]string) error{"proxy": proxyCmd, "welcome": welcomeCmd, "killat": killatCmd, "ackcheck": ackcheckCmd}
	run, ok := cmds[os.Args[1]]
	if !ok {
		fatal(fmt.Errorf("unknown subcommand %q", os.Args[1]))
	}
	if err := run(os.Args[2:]); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "crashdrv:", err)
	os.Exit(1)
}

func upFlags(fs *flag.FlagSet) (serverKey, keyFile *string) {
	return fs.String("server-key", "", "the upstream server's key (required)"),
		fs.String("key-file", "", "this tool's key file, created if missing (required)")
}

func dialUp(addr, serverKey string, priv ed25519.PrivateKey) (pushpb.PushClient, error) {
	conn, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return qnet.Dial(ctx, addr, qnet.Credential{Key: priv}, serverKey, qnet.ALPNPush)
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsg), grpc.MaxCallSendMsgSize(maxMsg)))
	if err != nil {
		return nil, err
	}
	return pushpb.NewPushClient(conn), nil
}

type logger struct {
	mu sync.Mutex
	f  *os.File
}

func (l *logger) line(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.f, "%d\t"+format+"\n", append([]any{time.Now().UnixMicro()}, args...)...)
}

type proxy struct {
	pushpb.UnimplementedPushServer
	up     pushpb.PushClient
	log    *logger
	serial bool
	hold   string
	k      int
	marker string
}

func proxyCmd(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7893", "UDP address the push client dials; it must pin the key printed on stderr")
	upstream := fs.String("upstream", "127.0.0.1:7892", "serve's QUIC address")
	serverKey, keyFile := upFlags(fs)
	logPath := fs.String("log", "", "event log (required)")
	serial := fs.Bool("serial", false, "forward an Append only after the previous one's Ack arrived")
	hold := fs.String("hold", "", "kill point: welcome2, acks (stop forwarding Acks after -k), append (hold Append k+1 after Ack k reached the client)")
	k := fs.Int("k", 0, "the append count the hold applies after")
	marker := fs.String("marker", "", "file created once the hold is in place")
	fs.Parse(args)
	if *logPath == "" || (*hold != "" && *marker == "") {
		return errors.New("proxy: -log, and -marker with -hold, are required")
	}
	if *hold == "append" && !*serial {
		return errors.New("proxy: -hold append needs -serial")
	}
	f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	priv, err := qnet.LoadOrCreateKey(*keyFile)
	if err != nil {
		return err
	}
	up, err := dialUp(*upstream, *serverKey, priv)
	if err != nil {
		return err
	}
	ln, err := qnet.Listen(*listen, priv, func(ed25519.PublicKey, []byte) (auth.Identity, bool) { return auth.Identity{}, true })
	if err != nil {
		return err
	}
	defer ln.Close()
	fmt.Fprintln(os.Stderr, "proxy key", qnet.PublicKey(priv))
	gs := grpc.NewServer(grpc.Creds(qnet.Credentials()), grpc.MaxRecvMsgSize(maxMsg),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}))
	pushpb.RegisterPushServer(gs, &proxy{up: up, log: &logger{f: f}, serial: *serial, hold: *hold, k: *k, marker: *marker})
	return gs.Serve(ln.Push)
}

func (p *proxy) mark(what string) {
	p.log.line("hold\t%s", what)
	if err := os.WriteFile(p.marker, nil, 0o644); err != nil {
		p.log.line("marker_error\t%v", err)
	}
}

func (p *proxy) Sync(down grpc.BidiStreamingServer[pushpb.ClientMsg, pushpb.ServerMsg]) error {
	ctx := down.Context()
	uctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-ctx.Done(); cancel() }()
	up, err := p.up.Sync(uctx)
	if err != nil {
		return err
	}
	p.log.line("stream_open")
	credit := make(chan struct{}, 1)
	credit <- struct{}{}
	var mu sync.Mutex
	ackedToClient, heldAppend, marked := 0, false, false
	markAppend := func() {
		if heldAppend && ackedToClient >= p.k && !marked {
			marked = true
			p.mark(fmt.Sprintf("append_after_ack\t%d", p.k))
		}
	}
	go func() {
		sent := 0
		for {
			m, err := down.Recv()
			if err != nil {
				p.log.line("client_recv_end\t%v", err)
				up.CloseSend()
				return
			}
			if a := m.GetAppend(); a != nil {
				if p.serial {
					select {
					case <-credit:
					case <-uctx.Done():
						return
					}
				}
				if p.hold == "append" && sent >= p.k {
					mu.Lock()
					heldAppend = true
					markAppend()
					mu.Unlock()
					<-uctx.Done()
					return
				}
				sent++
				p.log.line("append\t%d\t%s\t%d\t%d\t%d", a.Seq, a.SrcId, a.Offset, len(a.Body), a.RawEnd)
			}
			if err := up.Send(m); err != nil {
				p.log.line("upstream_send_end\t%v", err)
				return
			}
		}
	}()
	pages, acks, held := 0, 0, 0
	for {
		m, err := up.Recv()
		if err != nil {
			p.log.line("upstream_recv_end\t%v", err)
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if w := m.GetWelcome(); w != nil {
			pages++
			p.log.line("welcome\t%d\t%d\t%t", pages, len(w.Sources), w.More)
			if p.hold == "welcome2" && pages == 2 {
				p.mark("welcome_page\t2")
				<-ctx.Done()
				return ctx.Err()
			}
		}
		if a := m.GetAck(); a != nil {
			acks++
			st := a.GetState()
			p.log.line("ack\t%d\t%s\t%s\t%d", a.Seq, a.Result, st.GetSrcId(), st.GetSize())
			if p.serial {
				credit <- struct{}{}
			}
			if p.hold == "acks" && acks > p.k {
				held++
				p.log.line("ack_held\t%d", a.Seq)
				if held == 2 {
					p.mark(fmt.Sprintf("acks_unread\t%d", held))
				}
				continue
			}
		}
		if err := down.Send(m); err != nil {
			p.log.line("client_send_end\t%v", err)
			return err
		}
		if m.GetAck() != nil && p.hold == "append" {
			mu.Lock()
			ackedToClient++
			markAppend()
			mu.Unlock()
		}
	}
}

func welcomeCmd(args []string) error {
	fs := flag.NewFlagSet("welcome", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7892", "serve's QUIC address")
	serverKey, keyFile := upFlags(fs)
	machine := fs.String("machine", "", "Hello.machine")
	fs.Parse(args)
	priv, err := qnet.LoadOrCreateKey(*keyFile)
	if err != nil {
		return err
	}
	up, err := dialUp(*addr, *serverKey, priv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := up.Sync(ctx)
	if err != nil {
		return err
	}
	if err := s.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Hello{Hello: &pushpb.Hello{Machine: *machine}}}); err != nil {
		return err
	}
	var rows []string
	for {
		m, err := s.Recv()
		if err != nil {
			return err
		}
		w := m.GetWelcome()
		if w == nil {
			return errors.New("welcome: got a non-Welcome message")
		}
		for _, src := range w.Sources {
			rows = append(rows, fmt.Sprintf("%s\t%d", src.SrcId, src.Size))
		}
		if !w.More {
			break
		}
	}
	s.CloseSend()
	sort.Strings(rows)
	for _, r := range rows {
		fmt.Println(r)
	}
	return nil
}

type call struct {
	tid   int
	name  string
	text  string
	start float64
	dur   float64
	done  bool
}

var (
	lineRe    = regexp.MustCompile(`^(\d+)\s+(\d+\.\d+)\s+(.*)$`)
	callRe    = regexp.MustCompile(`^([a-z0-9_]+)\(`)
	resumedRe = regexp.MustCompile(`^<\.\.\. ([a-z0-9_]+) resumed>(.*)$`)
	durRe     = regexp.MustCompile(`<(\d+\.\d+)>$`)
)

type straceParser struct {
	pending map[int]*call
}

func (p *straceParser) feed(line string) *call {
	m := lineRe.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	tid, _ := strconv.Atoi(m[1])
	ts, _ := strconv.ParseFloat(m[2], 64)
	rest := m[3]
	if r := resumedRe.FindStringSubmatch(rest); r != nil {
		c := p.pending[tid]
		delete(p.pending, tid)
		if c == nil || c.name != r[1] {
			return nil
		}
		c.text += r[2]
		c.dur = parseDur(rest)
		c.done = true
		return c
	}
	cm := callRe.FindStringSubmatch(rest)
	if cm == nil {
		return nil
	}
	c := &call{tid: tid, name: cm[1], start: ts}
	if s, ok := strings.CutSuffix(rest, " <unfinished ...>"); ok {
		c.text = s
		p.pending[tid] = c
		return nil
	}
	c.text = rest
	c.dur = parseDur(rest)
	c.done = true
	return c
}

func parseDur(s string) float64 {
	if m := durRe.FindStringSubmatch(s); m != nil {
		d, _ := strconv.ParseFloat(m[1], 64)
		return d
	}
	return 0
}

var sysNrs = map[string][]int{"syncfs": {306}, "rename": {82, 264, 316}}

type need struct {
	re *regexp.Regexp
	n  int
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func killatCmd(args []string) error {
	fs := flag.NewFlagSet("killat", flag.ExitOnError)
	pid := fs.Int("pid", 0, "the process to SIGKILL (serve, started by crash.sh)")
	logPath := fs.String("strace", "", "strace -f -ttt -T -y log of pid, growing")
	sys := fs.String("sys", "syncfs", "held syscall: syncfs or rename (rename, renameat, renameat2)")
	target := fs.String("target", "", "rename only: the held call's new path must match this regexp")
	var needs multiFlag
	fs.Var(&needs, "need", "regexp a completed strace call must match since the last completed syncfs; repeatable, all must hold")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up after")
	debug := fs.Bool("debug", false, "print the counters every second on stderr")
	fs.Parse(args)
	nrs, ok := sysNrs[*sys]
	if !ok || *pid == 0 || *logPath == "" {
		return errors.New("killat: -pid, -strace and -sys syncfs|rename are required")
	}
	var tre *regexp.Regexp
	if *target != "" {
		tre = regexp.MustCompile(*target)
	}
	var ns []*need
	for _, n := range needs {
		ns = append(ns, &need{re: regexp.MustCompile(n)})
	}
	f, err := waitOpen(*logPath, *timeout)
	if err != nil {
		return err
	}
	rd := bufio.NewReader(f)
	sp := &straceParser{pending: map[int]*call{}}
	var partial string
	var lastBarrier float64
	type cand struct {
		tid   int
		since time.Time
		done  int
	}
	var cur *cand
	doneBy := map[int]int{}
	deadline := time.Now().Add(*timeout)
	lastDebug := time.Now()
	for time.Now().Before(deadline) {
		if *debug && time.Since(lastDebug) > time.Second {
			lastDebug = time.Now()
			counts := make([]string, len(ns))
			for i, n := range ns {
				counts[i] = strconv.Itoa(n.n)
			}
			tid, path := heldThread(*pid, nrs, tre != nil)
			fmt.Fprintf(os.Stderr, "debug\tneeds=%s\tcalls=%d\theld_tid=%d\theld_path=%q\n", strings.Join(counts, ","), len(doneBy), tid, path)
		}
		for {
			s, err := rd.ReadString('\n')
			if err != nil {
				partial += s
				break
			}
			c := sp.feed(strings.TrimRight(partial+s, "\n"))
			partial = ""
			if c == nil {
				continue
			}
			doneBy[c.tid]++
			if c.name == "syncfs" {
				lastBarrier = c.start
				for _, n := range ns {
					n.n = 0
				}
				continue
			}
			for _, n := range ns {
				if n.re.MatchString(c.text) {
					n.n++
				}
			}
		}
		ready := true
		for _, n := range ns {
			ready = ready && n.n > 0
		}
		if !ready {
			cur = nil
			time.Sleep(2 * time.Millisecond)
			continue
		}
		tid, path := heldThread(*pid, nrs, tre != nil)
		if tid == 0 || (tre != nil && !tre.MatchString(path)) {
			cur = nil
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if cur == nil || cur.tid != tid || cur.done != doneBy[tid] {
			cur = &cand{tid: tid, since: time.Now(), done: doneBy[tid]}
		} else if time.Since(cur.since) >= 20*time.Millisecond {
			if err := syscall.Kill(*pid, syscall.SIGKILL); err != nil {
				return err
			}
			counts := make([]string, len(ns))
			for i, n := range ns {
				counts[i] = strconv.Itoa(n.n)
			}
			fmt.Printf("killed\tpid=%d\ttid=%d\tsys=%s\tneed_counts=%s\tlast_barrier_ts=%.6f\n", *pid, tid, *sys, strings.Join(counts, ","), lastBarrier)
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return errors.New("killat: timed out before the kill point")
}

func waitOpen(path string, timeout time.Duration) (*os.File, error) {
	end := time.Now().Add(timeout)
	for {
		f, err := os.Open(path)
		if err == nil || time.Now().After(end) {
			return f, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func heldThread(pid int, nrs []int, wantPath bool) (int, string) {
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return 0, ""
	}
	for _, t := range tasks {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/syscall", pid, t.Name()))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(b))
		if len(fields) < 7 {
			continue
		}
		nr, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		for _, want := range nrs {
			if nr != want {
				continue
			}
			tid, _ := strconv.Atoi(t.Name())
			if !wantPath {
				return tid, ""
			}
			arg := 4
			if nr == 82 {
				arg = 2
			}
			addr, err := strconv.ParseUint(strings.TrimPrefix(fields[arg], "0x"), 16, 64)
			if err != nil {
				continue
			}
			return tid, readCString(pid, addr)
		}
	}
	return 0, ""
}

func readCString(pid int, addr uint64) string {
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := f.ReadAt(buf, int64(addr))
	s := string(buf[:n])
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return s
}

func ackcheckCmd(args []string) error {
	fs := flag.NewFlagSet("ackcheck", flag.ExitOnError)
	logPath := fs.String("strace", "", "serve's strace -f -ttt -T -y log")
	proxyLog := fs.String("proxy", "", "the serial proxy's log")
	manRe := fs.String("manifest", `, "[^"]*/raw/[^"]*/([0-9a-f]{16})\.json"\)`, "regexp of a manifest rename; group 1 is the src_id")
	fs.Parse(args)
	mre := regexp.MustCompile(*manRe)
	f, err := os.Open(*logPath)
	if err != nil {
		return err
	}
	defer f.Close()
	sp := &straceParser{pending: map[int]*call{}}
	type rn struct {
		src string
		end float64
	}
	var renames []rn
	var barriers [][2]float64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		c := sp.feed(sc.Text())
		if c == nil {
			continue
		}
		if c.name == "syncfs" {
			barriers = append(barriers, [2]float64{c.start, c.start + c.dur})
			continue
		}
		if strings.HasPrefix(c.name, "rename") && strings.Contains(c.text, ") = 0") {
			if m := mre.FindStringSubmatch(c.text); m != nil {
				renames = append(renames, rn{src: m[1], end: c.start + c.dur})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	sort.Slice(renames, func(i, j int) bool { return renames[i].end < renames[j].end })
	sort.Slice(barriers, func(i, j int) bool { return barriers[i][0] < barriers[j][0] })
	pf, err := os.Open(*proxyLog)
	if err != nil {
		return err
	}
	defer pf.Close()
	type ackRec struct {
		t   float64
		src string
	}
	var acks []ackRec
	conflicts := 0
	ps := bufio.NewScanner(pf)
	for ps.Scan() {
		x := strings.Split(ps.Text(), "\t")
		if len(x) < 6 || x[1] != "ack" {
			continue
		}
		if x[3] != "APPLIED" {
			conflicts++
			continue
		}
		us, _ := strconv.ParseInt(x[0], 10, 64)
		acks = append(acks, ackRec{t: float64(us) / 1e6, src: x[4]})
	}
	if len(acks) != len(renames) {
		return fmt.Errorf("ackcheck: %d APPLIED acks but %d manifest renames", len(acks), len(renames))
	}
	violations, srcMismatch := 0, 0
	var gaps []float64
	for i, a := range acks {
		r := renames[i]
		if r.src != a.src {
			srcMismatch++
		}
		j := sort.Search(len(barriers), func(j int) bool { return barriers[j][0] >= r.end })
		if j == len(barriers) {
			violations++
			continue
		}
		g := a.t - barriers[j][1]
		gaps = append(gaps, g)
		if g <= 0 {
			violations++
		}
	}
	sort.Float64s(gaps)
	if len(gaps) == 0 {
		return errors.New("ackcheck: no acks paired")
	}
	fmt.Printf("acks_checked\t%d\nconflict_acks\t%d\nsrc_mismatch\t%d\nviolations\t%d\nmin_gap_us\t%.0f\nmedian_gap_us\t%.0f\nmax_gap_us\t%.0f\nbarriers\t%d\n",
		len(acks), conflicts, srcMismatch, violations, gaps[0]*1e6, gaps[len(gaps)/2]*1e6, gaps[len(gaps)-1]*1e6, len(barriers))
	if violations > 0 || srcMismatch > 0 {
		return errors.New("ackcheck: FAIL")
	}
	return nil
}
