package main

// SoundPEATS control protocol over SPP (RFCOMM channel 1, UUID 0x1101), from
// the PeatsAudio app (com.xingkeqi.peats 1.9.45) and checked against a T3 Pro:
//
//	request  FF 04 00 <len> 00 0A <cmd:2> <payload>
//	reply    FF 04 00 <len> 00 0A <cmd|0x8000:2> <payload>
//
// Get replies carry [status, value]. Sets are not acknowledged, so state is
// only ever taken from a read.

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

const (
	cmdBatteryLeft  = 0x0306
	cmdBatteryRight = 0x0307
	cmdFirmware     = 0x0309
	cmdModeGet      = 0x0310
	cmdModeSet      = 0x0311
	cmdPromptGet    = 0x0319
	cmdPromptSet    = 0x0320
	cmdBatteryCase  = 0x0323
	cmdKeysGet      = 0x03AA
	cmdKeysSet      = 0x03AB
	cmdKeysReset    = 0x03AC
	replyBit        = 0x8000
)

// On/off settings: name -> get command. The set command is get+1.
var switches = map[string]uint16{
	"game":       0x030E,
	"touch-lock": 0x0312,
	"movie":      0x0327,
	"dynamic-eq": 0x0331,
}

// Index = wire code. Only the first three modes are settable; the rest are
// names other models report.
var modes = []string{"off", "anc", "transparency", "adaptive", "weak"}

var gestures = []string{"", "tap-left", "tap-right", "double-left", "double-right",
	"triple-left", "triple-right", "hold-left", "hold-right"}

var actions = []string{"", "volume-up", "volume-down", "play-pause", "game-mode",
	"previous", "next", "anc", "assistant"}

var vendor = []byte{0x00, 0x0A}

func frame(cmd uint16, payload ...byte) []byte {
	return append([]byte{0xFF, 0x04, 0x00, byte(len(payload)), 0x00, 0x0A, byte(cmd >> 8), byte(cmd)}, payload...)
}

func switchCommands() (cmds []uint16) {
	for _, name := range slices.Sorted(maps.Keys(switches)) {
		cmds = append(cmds, switches[name])
	}
	return cmds
}

// Mode last: replies come in order, so a known mode means the rest arrived.
func startupQueries() []uint16 {
	q := []uint16{cmdFirmware, cmdBatteryLeft, cmdBatteryRight, cmdBatteryCase, cmdPromptGet, cmdKeysGet}
	return append(append(q, switchCommands()...), cmdModeGet)
}

func pollQueries() []uint16 { return append(switchCommands(), cmdModeGet) }

func batteryQueries() []uint16 { return []uint16{cmdBatteryLeft, cmdBatteryRight, cmdBatteryCase} }

// frameReader splits the RFCOMM byte stream into frames. Replies can arrive
// glued together or split across reads.
type frameReader struct{ buf []byte }

func (r *frameReader) feed(chunk []byte) (frames [][]byte) {
	r.buf = append(r.buf, chunk...)
	for {
		start := bytes.IndexByte(r.buf, 0xFF)
		if start < 0 {
			r.buf = r.buf[:0]
			return frames
		}
		r.buf = r.buf[start:]
		if len(r.buf) < 6 {
			return frames
		}
		if !bytes.Equal(r.buf[4:6], vendor) {
			r.buf = r.buf[1:]
			continue
		}
		size := 8 + int(r.buf[3])
		if r.buf[2]&1 != 0 {
			size++
		}
		if len(r.buf) < size {
			return frames
		}
		frames = append(frames, bytes.Clone(r.buf[:size]))
		r.buf = r.buf[size:]
	}
}

// decode returns the command and payload of a reply frame.
func decode(f []byte) (uint16, []byte, bool) {
	if len(f) < 8 || f[0] != 0xFF || !bytes.Equal(f[4:6], vendor) {
		return 0, nil, false
	}
	cmd := uint16(f[6])<<8 | uint16(f[7])
	if cmd&replyBit == 0 {
		return 0, nil, false
	}
	return cmd &^ replyBit, f[8:min(len(f), 8+int(f[3]))], true
}

type Battery struct {
	Left  *int `json:"left"`
	Right *int `json:"right"`
	Case  *int `json:"case"`
}

type State struct {
	Connected    bool              `json:"connected"`
	Control      bool              `json:"control"`
	Address      string            `json:"address"`
	Name         string            `json:"name"`
	Firmware     string            `json:"firmware"`
	Mode         string            `json:"mode"`
	Switches     map[string]bool   `json:"switches"`
	PromptVolume *int              `json:"promptVolume"`
	Keys         map[string]string `json:"keys"`
	Battery      Battery           `json:"battery"`
	Headset      *int              `json:"headset"`
}

// value reads a [status, value] get reply. Non-zero status means no answer.
func value(p []byte) (int, bool) {
	switch {
	case len(p) >= 2 && p[0] == 0:
		return int(p[1]), true
	case len(p) == 1:
		return int(p[0]), true
	}
	return 0, false
}

func percent(p []byte) *int {
	if v, ok := value(p); ok && v <= 100 {
		return &v
	}
	return nil
}

func name(list []string, code int) string {
	if code >= 0 && code < len(list) && list[code] != "" {
		return list[code]
	}
	return fmt.Sprintf("code%d", code)
}

// apply folds one reply into the state.
func (s *State) apply(cmd uint16, p []byte) {
	switch cmd {
	case cmdBatteryLeft:
		s.Battery.Left = percent(p)
	case cmdBatteryRight:
		s.Battery.Right = percent(p)
	case cmdBatteryCase:
		s.Battery.Case = percent(p)
	case cmdPromptGet:
		s.PromptVolume = percent(p)
	case cmdModeGet:
		if v, ok := value(p); ok {
			s.Mode = name(modes, v)
		}
	case cmdFirmware:
		s.Firmware = strings.Trim(string(bytes.TrimPrefix(p, []byte{0})), "\x00 ")
	case cmdKeysGet:
		// [gesture, action] pairs, no status byte on this chip.
		if len(p) == 0 || len(p)%2 != 0 {
			return
		}
		s.Keys = map[string]string{}
		for i := 0; i < len(p); i += 2 {
			if p[i] != 0 {
				s.Keys[name(gestures, int(p[i]))] = name(actions, int(p[i+1]))
			}
		}
	default:
		for sw, get := range switches {
			if cmd == get {
				if v, ok := value(p); ok {
					if s.Switches == nil {
						s.Switches = map[string]bool{}
					}
					s.Switches[sw] = v == 1
				}
			}
		}
	}
}

// get returns a setting as the text a command would set it to.
func (s *State) get(field string) string {
	switch {
	case field == "mode":
		return s.Mode
	case field == "prompt-volume":
		if s.PromptVolume == nil {
			return ""
		}
		return strconv.Itoa(*s.PromptVolume)
	case strings.HasPrefix(field, "key:"):
		return s.Keys[field[4:]]
	}
	if on, ok := s.Switches[field]; ok {
		return map[bool]string{true: "on", false: "off"}[on]
	}
	return ""
}

func (s *State) ready() bool { return s.Control && s.Mode != "" }

// plan turns one command into frames, plus the setting it changes and the
// value a read-back should show. field is "" for commands with no read-back.
func plan(s *State, args []string) (frames [][]byte, field, want string, err error) {
	if len(args) == 0 {
		return nil, "", "", fmt.Errorf("no command")
	}
	cmd, n := args[0], len(args)
	switch {
	case n == 1 && (cmd == "status" || cmd == "keys"):
		return nil, "", "", nil
	case n == 1 && cmd == "refresh":
		for _, c := range startupQueries() {
			frames = append(frames, frame(c))
		}
		return frames, "", "", nil
	case n == 2 && cmd == "mode":
		want = args[1]
		if want == "next" {
			want = modes[(slices.Index(modes[:3], s.Mode)+1)%3]
		}
		code := slices.Index(modes[:3], want)
		if code < 0 {
			return nil, "", "", fmt.Errorf("mode must be off, anc, transparency or next")
		}
		return [][]byte{frame(cmdModeSet, byte(code)), frame(cmdModeGet)}, "mode", want, nil
	case n == 2 && cmd == "prompt-volume":
		v, err := strconv.Atoi(args[1])
		if err != nil || v < 0 || v > 100 {
			return nil, "", "", fmt.Errorf("prompt-volume must be 0-100")
		}
		// The buds round down to 16 steps (40 reads back as 37), so there is
		// no exact read-back to confirm.
		return [][]byte{frame(cmdPromptSet, byte(v)), frame(cmdPromptGet)}, "", "", nil
	case n == 3 && cmd == "key":
		g, a := slices.Index(gestures, args[1]), slices.Index(actions, args[2])
		if g < 1 || a < 1 {
			return nil, "", "", fmt.Errorf("key <%s> <%s>",
				strings.Join(gestures[1:], "|"), strings.Join(actions[1:], "|"))
		}
		return [][]byte{frame(cmdKeysSet, byte(g), byte(a)), frame(cmdKeysGet)}, "key:" + args[1], args[2], nil
	case n == 2 && cmd == "keys" && args[1] == "reset":
		return [][]byte{frame(cmdKeysReset), frame(cmdKeysGet)}, "", "", nil
	case n == 2 && switches[cmd] != 0:
		if args[1] != "on" && args[1] != "off" {
			return nil, "", "", fmt.Errorf("%s must be on or off", cmd)
		}
		get := switches[cmd]
		on := byte(0)
		if args[1] == "on" {
			on = 1
		}
		return [][]byte{frame(get+1, on), frame(get)}, cmd, args[1], nil
	}
	return nil, "", "", fmt.Errorf("unknown command: %s", strings.Join(args, " "))
}
