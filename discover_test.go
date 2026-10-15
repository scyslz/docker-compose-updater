package main

import "testing"

func TestPrimaryComposeFile(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/docker-compose.yml", "/a/docker-compose.yml"},
		{"/a/compose.yml,/a/compose.override.yml", "/a/compose.yml"},
		{" /a/compose.yml , /b/compose.yml ", "/a/compose.yml"},
		{"", ""},
		{"  ", ""},
	}
	for _, c := range cases {
		if got := primaryComposeFile(c.in); got != c.want {
			t.Errorf("primaryComposeFile(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
