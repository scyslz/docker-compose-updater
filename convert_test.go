package main

import "testing"

func TestComposePortSpec(t *testing.T) {
	cases := []struct{ hostIP, hostPort, containerPort, proto, want string }{
		{"", "8080", "80", "tcp", "8080:80"},
		{"0.0.0.0", "8080", "80", "tcp", "8080:80"},
		{"::", "8080", "80", "tcp", "8080:80"},
		{"127.0.0.1", "8080", "80", "tcp", "127.0.0.1:8080:80"},
		{"", "53", "53", "udp", "53:53/udp"},
		{"127.0.0.1", "5353", "53", "udp", "127.0.0.1:5353:53/udp"},
	}
	for _, c := range cases {
		if got := composePortSpec(c.hostIP, c.hostPort, c.containerPort, c.proto); got != c.want {
			t.Errorf("composePortSpec(%q, %q, %q, %q) = %q, want %q",
				c.hostIP, c.hostPort, c.containerPort, c.proto, got, c.want)
		}
	}
}

func TestParsePublishedPorts(t *testing.T) {
	out := []byte("web\t0.0.0.0:8080->80/tcp, :::8080->80/tcp\n" +
		"dns\t0.0.0.0:53->53/udp\n" +
		"range\t0.0.0.0:7000-7001->80-81/tcp\n" +
		"none\t\n" +
		"self\t0.0.0.0:9000->9000/tcp\n")
	got := parsePublishedPorts(out, "self")
	want := map[string]string{
		"8080/tcp": "web",
		"53/udp":   "dns",
		"7000/tcp": "range",
		"7001/tcp": "range",
	}
	if len(got) != len(want) {
		t.Fatalf("parsePublishedPorts = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parsePublishedPorts[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestNextFreePort(t *testing.T) {
	occupied := map[int]bool{8080: true, 8081: true, 8083: true}
	if got := nextFreePort(8080, occupied); got != 8082 {
		t.Errorf("nextFreePort = %d, want 8082", got)
	}
	// Wrap around when the top of the range is exhausted.
	top := map[int]bool{65535: true}
	if got := nextFreePort(65535, top); got != 1024 {
		t.Errorf("nextFreePort wrap = %d, want 1024", got)
	}
}

func TestExpandPortRange(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"8080", []string{"8080"}},
		{"7000-7001", []string{"7000", "7001"}},
		{"x-y", []string{"x-y"}},
	}
	for _, c := range cases {
		got := expandPortRange(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("expandPortRange(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("expandPortRange(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestServiceName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"my-app", "my-app"},
		{"My_App.1", "my_app.1"},
		{"-weird name-", "weird-name"},
		{"", "converted"},
	}
	for _, c := range cases {
		if got := serviceName(c.in); got != c.want {
			t.Errorf("serviceName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
