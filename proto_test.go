package main

import (
	"encoding/hex"
	"testing"
)

// Frames captured from a T3 Pro on firmware ZKLXT3PRO_20250428_v0.0.31.
func h(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

var (
	modeTransparency = h("ff040002000a83100002")
	left100          = h("ff040002000a83060064")
	case15           = h("ff040002000a8323000f")
	gameOff          = h("ff040002000a830e0000")
	prompt37         = h("ff040002000a83190025")
	keysDefault      = h("ff040010000a83aa01020201030304030505060607070807")
	firmware         = append(h("ff04001a000a8309"), "ZKLXT3PRO_20250428_v0.0.31"...)
)

func TestRequests(t *testing.T) {
	s := &State{Mode: "transparency"}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"mode", "anc"}, "ff040001000a031101"},
		{[]string{"mode", "next"}, "ff040001000a031100"}, // transparency -> off
		{[]string{"game", "on"}, "ff040001000a030f01"},
		{[]string{"touch-lock", "off"}, "ff040001000a031300"},
		{[]string{"prompt-volume", "37"}, "ff040001000a032025"},
		{[]string{"key", "tap-left", "next"}, "ff040002000a03ab0106"},
		{[]string{"keys", "reset"}, "ff040000000a03ac"},
	} {
		frames, _, _, err := plan(s, c.args)
		if err != nil || hex.EncodeToString(frames[0]) != c.want {
			t.Errorf("%v: got %x, %v; want %s", c.args, frames, err, c.want)
		}
	}
	for _, bad := range [][]string{{"mode", "loud"}, {"key", "tap-left", "explode"}, {"prompt-volume", "101"}, {"reset"}, {"game"}} {
		if _, _, _, err := plan(s, bad); err == nil {
			t.Errorf("%v: want error", bad)
		}
	}
}

func TestFrameReader(t *testing.T) {
	var r frameReader
	glued := append(append([]byte{0x01, 0x02}, modeTransparency...), keysDefault[:5]...)
	if got := r.feed(glued); len(got) != 1 || string(got[0]) != string(modeTransparency) {
		t.Fatalf("glued: %x", got)
	}
	if got := r.feed(keysDefault[5:]); len(got) != 1 || string(got[0]) != string(keysDefault) {
		t.Fatalf("split: %x", got)
	}
}

func TestApply(t *testing.T) {
	s := &State{Control: true}
	for _, f := range [][]byte{left100, case15, gameOff, prompt37, keysDefault, firmware, modeTransparency} {
		cmd, p, ok := decode(f)
		if !ok {
			t.Fatalf("decode %x", f)
		}
		s.apply(cmd, p)
	}
	if !s.ready() || s.Mode != "transparency" || *s.Battery.Left != 100 || *s.Battery.Case != 15 ||
		s.Battery.Right != nil || s.get("game") != "off" || s.get("prompt-volume") != "37" ||
		s.Firmware != "ZKLXT3PRO_20250428_v0.0.31" {
		t.Fatalf("state: %+v", s)
	}
	want := map[string]string{
		"tap-left": "volume-down", "tap-right": "volume-up",
		"double-left": "play-pause", "double-right": "play-pause",
		"triple-left": "previous", "triple-right": "next",
		"hold-left": "anc", "hold-right": "anc",
	}
	for g, a := range want {
		if s.Keys[g] != a {
			t.Errorf("%s = %s, want %s", g, s.Keys[g], a)
		}
	}
	s.apply(cmdModeGet, []byte{1, 0}) // non-zero status: no answer
	if s.Mode != "transparency" {
		t.Errorf("error status changed mode to %s", s.Mode)
	}
	if _, _, ok := decode(frame(cmdModeGet)); ok {
		t.Error("a request decoded as a reply")
	}
}
