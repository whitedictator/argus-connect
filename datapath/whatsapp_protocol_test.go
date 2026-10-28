package datapath

import "testing"

func TestWhatsAppLongClientHello(t *testing.T) {
	p := []byte{'W', 'A', 6, 3, 0, 7, 131, 0x12, 0x80, 0x0f, 0x0a, 32}
	if !isWhatsAppNoiseStart(p) {
		t.Fatal("long hello rejected")
	}
	p[6]++
	if isWhatsAppNoiseStart(p) {
		t.Fatal("inconsistent frame accepted")
	}
}

func TestWhatsAppNoisePreface(t *testing.T) {
	p := []byte{'W', 'A', 6, 3, 0, 0, 36, 0x12, 34, 0x0a, 32}
	if !isWhatsAppNoiseStart(p) {
		t.Fatal("missing known preface")
	}
	for n := 0; n < len(p); n++ {
		if isWhatsAppNoiseStart(p[:n]) {
			t.Fatal("truncated preface accepted")
		}
	}
	for _, i := range []int{0, 1, 2, 3, 7, 8, 9, 10} {
		q := append([]byte(nil), p...)
		q[i] ^= 1
		if isWhatsAppNoiseStart(q) {
			t.Fatalf("bad field %d accepted", i)
		}
	}
	if isWhatsAppNoiseStart([]byte("GET / HTTP/1.1\r\n")) {
		t.Fatal("HTTP misclassified")
	}
}
