// peats controls SoundPEATS earbuds over Bluetooth Classic SPP.
//
// The buds take one SPP client. `peats watch` holds the channel, prints a
// JSON line per state change, and takes commands on stdin and on an abstract
// Unix socket. Every other command goes through a running watcher, or talks
// to the buds directly when none is running.
package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	hubSocket     = "@hamid.soundpeats"
	pollEvery     = 4 * time.Second
	batteryEvery  = 60 * time.Second
	silenceLimit  = 15 * time.Second
	retryEvery    = 5 * time.Second
	writeGap      = 50 * time.Millisecond // the app waits this long after each write
	confirmWithin = 4 * time.Second
)

var channel = flag.Int("channel", 1, "RFCOMM channel of the buds' SPP service")

const usage = `usage: peats [-channel N] <command>

  status                         print the current state as JSON
  mode off|anc|transparency|next noise control
  game|touch-lock|movie|dynamic-eq on|off
  prompt-volume 0-100            voice prompt volume
  keys                           list gesture -> action
  key <gesture> <action>         set one gesture
  keys reset                     restore default gestures
  refresh                        re-read everything
  watch                          hold the channel, print JSON lines (the bar runs this)

gestures: ` + "tap|double|triple|hold" + `-left|right
actions:  volume-up volume-down play-pause game-mode previous next anc assistant
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" {
		fmt.Print(usage)
		return
	}
	if args[0] == "watch" {
		watch()
		return
	}
	if _, _, _, err := plan(&State{}, args); err != nil {
		fmt.Fprintln(os.Stderr, "peats:", err)
		os.Exit(2)
	}
	os.Exit(run(args))
}

func logf(format string, a ...any) { fmt.Fprintf(os.Stderr, "peats: "+format+"\n", a...) }

// --- Bluetooth -------------------------------------------------------------

var deviceRe = regexp.MustCompile(`Device\s+([0-9A-Fa-f:]{17})\s+(.+)$`)
var batteryRe = regexp.MustCompile(`Battery Percentage:.*\((\d+)\)`)

func bluetoothctl(args ...string) string {
	out, _ := exec.Command("bluetoothctl", args...).Output()
	return string(out)
}

// findBuds returns the first connected device named SoundPEATS.
func findBuds() (addr, name string, ok bool) {
	for _, line := range strings.Split(bluetoothctl("devices", "Connected"), "\n") {
		m := deviceRe.FindStringSubmatch(strings.TrimSpace(line))
		if m != nil && strings.Contains(strings.ToLower(m[2]), "soundpeats") {
			return strings.ToUpper(m[1]), strings.TrimSpace(m[2]), true
		}
	}
	return "", "", false
}

// headsetBattery is the HFP battery BlueZ already knows, shown until SPP answers.
func headsetBattery(addr string) *int {
	m := batteryRe.FindStringSubmatch(bluetoothctl("info", addr))
	if m == nil {
		return nil
	}
	v, _ := strconv.Atoi(m[1])
	return &v
}

func dial(addr string) (*os.File, error) {
	var sa unix.SockaddrRFCOMM
	sa.Channel = uint8(*channel)
	for i, part := range strings.Split(addr, ":") {
		b, err := strconv.ParseUint(part, 16, 8)
		if err != nil || i > 5 {
			return nil, fmt.Errorf("bad address %q", addr)
		}
		sa.Addr[5-i] = byte(b) // bdaddr_t is little-endian
	}
	// EBUSY: the previous client's RFCOMM link is still closing. Retry briefly.
	for try := 0; ; try++ {
		fd, err := connect(&sa)
		if err == nil {
			// Non-blocking makes the File pollable, so Close unblocks a pending Read.
			return os.NewFile(uintptr(fd), "rfcomm"), nil
		}
		if err != unix.EBUSY || try == 10 {
			return nil, fmt.Errorf("rfcomm connect: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// connect does a non-blocking connect and waits for it. A blocking connect
// returns EINTR whenever the Go runtime signals the thread.
func connect(sa unix.Sockaddr) (int, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.BTPROTO_RFCOMM)
	if err != nil {
		return -1, err
	}
	err = unix.Connect(fd, sa)
	if err == unix.EINPROGRESS {
		deadline := time.Now().Add(10 * time.Second)
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		for {
			n, perr := unix.Poll(pfd, int(time.Until(deadline).Milliseconds()))
			if perr == unix.EINTR {
				continue
			}
			if perr != nil || n == 0 {
				err = cmp.Or(perr, error(unix.ETIMEDOUT))
				break
			}
			var soerr int
			if soerr, err = unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR); err == nil && soerr != 0 {
				err = unix.Errno(soerr)
			}
			break
		}
	}
	if err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// --- Session ---------------------------------------------------------------

type pending struct {
	want   string
	frames [][]byte // set frames, then the get that reads it back
	checks int
}

// session holds the SPP channel until it drops. It applies commands from
// cmds and calls emit after every change.
func session(addr string, s *State, cmds <-chan string, emit func(*State)) error {
	f, err := dial(addr)
	if err != nil {
		return err
	}
	defer f.Close()

	in, readErr := make(chan []byte, 32), make(chan error, 1)
	go func() {
		var r frameReader
		buf := make([]byte, 1024)
		for {
			n, err := f.Read(buf)
			if err != nil {
				readErr <- err
				return
			}
			for _, fr := range r.feed(buf[:n]) {
				in <- fr
			}
		}
	}()

	write := func(frames ...[]byte) error {
		for _, fr := range frames {
			if _, err := f.Write(fr); err != nil {
				return err
			}
			time.Sleep(writeGap)
		}
		return nil
	}
	query := func(cmds []uint16) error {
		for _, c := range cmds {
			if err := write(frame(c)); err != nil {
				return err
			}
		}
		return nil
	}

	if err := query(startupQueries()); err != nil {
		return err
	}
	s.Control = true
	emit(s)

	poll, battery := time.NewTicker(pollEvery), time.NewTicker(batteryEvery)
	defer poll.Stop()
	defer battery.Stop()
	lastReply := time.Now()
	// Sets are not acknowledged and apply after a get sent right behind them,
	// and the buds sometimes drop one. So read back after a moment, resend
	// once on mismatch, then give up.
	waiting := map[string]*pending{}
	var rechecks [][]byte
	var followup <-chan time.Time

	for {
		select {
		case fr := <-in:
			if cmd, p, ok := decode(fr); ok {
				lastReply = time.Now()
				s.apply(cmd, p)
				for field, w := range waiting {
					if s.get(field) == w.want {
						delete(waiting, field)
					}
				}
			}
		case err := <-readErr:
			return err
		case line := <-cmds:
			frames, field, want, err := plan(s, strings.Fields(line))
			if err != nil {
				logf("%v", err)
				break
			}
			if err := write(frames...); err != nil {
				return err
			}
			if len(frames) > 0 {
				rechecks = append(rechecks, frames[len(frames)-1])
				followup = time.After(500 * time.Millisecond)
			}
			if field != "" && s.get(field) != want {
				waiting[field] = &pending{want: want, frames: frames}
			}
		case <-followup:
			followup = nil
			// Entries still here did not match the last read-back.
			for field, w := range waiting {
				w.checks++
				switch w.checks {
				case 1:
				case 2:
					if err := write(w.frames[:len(w.frames)-1]...); err != nil {
						return err
					}
				default:
					logf("%s did not change to %s", field, w.want)
					delete(waiting, field)
					continue
				}
				rechecks = append(rechecks, w.frames[len(w.frames)-1])
			}
			if err := write(rechecks...); err != nil {
				return err
			}
			rechecks = nil
			if len(waiting) > 0 {
				followup = time.After(600 * time.Millisecond)
			}
		case <-poll.C:
			if time.Since(lastReply) > silenceLimit {
				return fmt.Errorf("buds stopped answering")
			}
			if err := query(pollQueries()); err != nil {
				return err
			}
		case <-battery.C:
			s.Headset = headsetBattery(addr)
			if err := query(batteryQueries()); err != nil {
				return err
			}
		}
		emit(s)
	}
}

// --- Watcher ---------------------------------------------------------------

// hub fans state lines out to stdout and socket clients.
type hub struct {
	mu      sync.Mutex
	last    string
	clients map[net.Conn]bool
}

func (h *hub) emit(s *State) {
	b, _ := json.Marshal(s)
	line := string(b) + "\n"
	h.mu.Lock()
	defer h.mu.Unlock()
	if line == h.last {
		return
	}
	h.last = line
	os.Stdout.WriteString(line)
	for c := range h.clients {
		c.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := c.Write([]byte(line)); err != nil {
			c.Close()
			delete(h.clients, c)
		}
	}
}

func (h *hub) add(c net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = true
	if h.last != "" {
		c.Write([]byte(h.last))
	}
}

func (h *hub) drop(c net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	c.Close()
}

// sameUser reports whether the process on the other end runs as us. The
// abstract socket namespace has no file permissions to lean on.
func sameUser(c net.Conn) bool {
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	raw.Control(func(fd uintptr) {
		cred, err = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	return err == nil && int(cred.Uid) == os.Getuid()
}

func lines(r io.Reader, out chan<- string) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		out <- sc.Text()
	}
}

func watch() {
	cmds := make(chan string, 16)
	go func() {
		lines(os.Stdin, cmds)
		os.Exit(0) // the bar went away
	}()
	for {
		ln, err := net.Listen("unix", hubSocket)
		if err != nil {
			mirror(cmds) // another watcher owns the buds
			continue
		}
		own(ln, cmds)
	}
}

// own serves clients and keeps a session to the buds alive. It never returns.
func own(ln net.Listener, cmds chan string) {
	h := &hub{clients: map[net.Conn]bool{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				continue
			}
			if !sameUser(c) {
				c.Close()
				continue
			}
			h.add(c)
			go func() {
				lines(c, cmds)
				h.drop(c)
			}()
		}
	}()
	for {
		s := &State{}
		if addr, name, ok := findBuds(); ok {
			*s = State{Connected: true, Address: addr, Name: name, Headset: headsetBattery(addr)}
			h.emit(s)
			if err := session(addr, s, cmds, h.emit); err != nil {
				logf("control channel: %v", err)
			}
			s.Control = false
		}
		h.emit(s)
		idle(cmds, retryEvery)
	}
}

// idle waits, dropping commands that arrive while there is no session.
func idle(cmds <-chan string, d time.Duration) {
	done := time.After(d)
	for {
		select {
		case <-cmds:
		case <-done:
			return
		}
	}
}

// mirror relays a running watcher's lines to stdout and our commands to it.
func mirror(cmds <-chan string) {
	c, err := dialHub()
	if err != nil {
		time.Sleep(time.Second)
		return
	}
	defer c.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case line := <-cmds:
				if _, err := c.Write([]byte(line + "\n")); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		fmt.Println(sc.Text())
	}
}

func dialHub() (net.Conn, error) {
	c, err := net.Dial("unix", hubSocket)
	if err != nil {
		return nil, err
	}
	if !sameUser(c) {
		c.Close()
		return nil, fmt.Errorf("%s is owned by another user", hubSocket)
	}
	return c, nil
}

// --- One-shot commands -----------------------------------------------------

func run(args []string) int {
	states, send := make(chan string, 16), func(string) {}
	if c, err := dialHub(); err == nil {
		defer c.Close()
		go func() {
			lines(c, states)
			close(states)
		}()
		send = func(line string) { c.Write([]byte(line + "\n")) }
	} else {
		addr, name, ok := findBuds()
		if !ok {
			logf("no SoundPEATS buds connected")
			return 1
		}
		cmds := make(chan string, 1)
		send = func(line string) { cmds <- line }
		go func() {
			s := &State{Connected: true, Address: addr, Name: name, Headset: headsetBattery(addr)}
			err := session(addr, s, cmds, func(s *State) {
				b, _ := json.Marshal(s)
				states <- string(b)
			})
			logf("control channel: %v", err)
			close(states)
		}()
	}
	return await(states, send, args)
}

// await waits for the buds to be ready, sends the command, and prints the
// state once a read-back confirms it.
func await(states <-chan string, send func(string), args []string) int {
	deadline := time.After(confirmWithin)
	var s State
	field, want, sent := "", "", false
	readOnly := len(args) == 1 && (args[0] == "status" || args[0] == "keys")
	for {
		select {
		case line, ok := <-states:
			if !ok {
				return 1
			}
			s = State{}
			json.Unmarshal([]byte(line), &s)
			if !s.ready() {
				continue
			}
			if !sent {
				_, field, want, _ = plan(&s, args)
				cmd := strings.Join(args, " ")
				if field == "mode" {
					cmd = "mode " + want // resolve "next" here, not in a racing watcher
				}
				if !readOnly {
					send(cmd)
				}
				sent = true
				if field == "" && !readOnly {
					deadline = time.After(1500 * time.Millisecond) // no read-back to wait for
					continue
				}
			}
			if readOnly || field != "" && s.get(field) == want {
				return report(&s, args)
			}
		case <-deadline:
			if !sent {
				logf("buds not answering (is the phone app connected?)")
				return 1
			}
			if field != "" {
				logf("%s did not change to %s", field, want)
				report(&s, args)
				return 1
			}
			return report(&s, args)
		}
	}
}

func report(s *State, args []string) int {
	if args[0] == "keys" && len(args) == 1 {
		for _, g := range gestures[1:] {
			fmt.Printf("%-13s %s\n", g, s.Keys[g])
		}
		return 0
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(b))
	return 0
}
