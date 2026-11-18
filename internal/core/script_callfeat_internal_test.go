package core

import (
	"strings"
	"testing"
)

func TestPageNumbers(t *testing.T) {
	for in, want := range map[string]string{"1000": "1000", "1000-1002": "1000,1001,1002", "0100-0102": "0100,0101,0102", "5-5": "5"} {
		got, err := pageNumbers(in)
		if err != nil || strings.Join(got, ",") != want {
			t.Errorf("pageNumbers(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"010-12", "5-1", "a-b", "1-99999"} {
		if _, err := pageNumbers(bad); err == nil {
			t.Errorf("pageNumbers(%q) should fail", bad)
		}
	}
}

func TestDigitSoundsAndForward(t *testing.T) {
	if got := strings.Join(digitSounds("+1 (20)5"), ","); got != "digits/1.wav,digits/2.wav,digits/0.wav,digits/5.wav" {
		t.Errorf("digitSounds: %s", got)
	}
	if !isNumber("12345") || isNumber("sip:x@y") || isNumber("") {
		t.Error("isNumber")
	}
}
