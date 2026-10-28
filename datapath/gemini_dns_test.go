package datapath

import (
	"net"
	"testing"
)

func TestGeminiDNSIsolation(t *testing.T) {
	for _, name := range []string{"gemini.google.com", "GEMINI.GOOGLE.COM.", "generativelanguage.googleapis.com", "robinfrontend-pa.googleapis.com", "notebook.google.com"} {
		if !isGeminiDNSName(name) {
			t.Errorf("missing Gemini name %s", name)
		}
	}
	for _, name := range []string{"youtube.googleapis.com", "youtubei.googleapis.com", "googlevideo.com", "telegram.org", "mmg.whatsapp.net", "threema.ch", "accounts.google.com", "gemini.google.com.attacker.example", "notgemini.google.com"} {
		if isGeminiDNSName(name) {
			t.Errorf("unrelated DNS intercepted: %s", name)
		}
	}
}

func TestNotebookDoesNotInheritGeminiTLSStrategy(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.enabled.Store(true)
	p.recordTCP(serviceGemini, strategyComboFast, true)
	if service := classifyDomain("notebook.google.com"); service != serviceNotebook {
		t.Fatalf("wrong class %s", service)
	}
	if got := p.choose(serviceNotebook, clientHello("notebook.google.com")); got != strategyDirect {
		t.Fatalf("wrong initial method %s", got)
	}
	if isGeminiDNSName("www.google.com") || isGeminiDNSName("accounts.google.com") {
		t.Fatal("ordinary Google redirected")
	}
}

func TestGeminiStrategyDoesNotReplaceWorkingServices(t *testing.T) {
	p := newLightPolicy(&net.Dialer{})
	defer p.mtproxies.cancel()
	p.enabled.Store(true)
	for _, service := range []serviceID{serviceYouTube, serviceTelegram, serviceWhatsApp, serviceThreema, serviceMeta} {
		p.recordTCP(service, strategyTLSRecord, true)
	}
	if got := p.choose(serviceGemini, clientHello("robinfrontend-pa.googleapis.com")); got != strategyComboFast {
		t.Fatalf("Gemini first strategy %s", got)
	}
	for _, service := range []serviceID{serviceYouTube, serviceTelegram, serviceWhatsApp, serviceThreema, serviceMeta} {
		if got := p.choose(service, clientHello("example.com")); got != strategyTLSRecord {
			t.Fatalf("working %s changed to %s", service, got)
		}
	}
}
