package docker

import (
	"slices"
	"testing"
	"time"
)

func TestParseInspectReportsUptimeFields(t *testing.T) {
	c, ok, err := parseInspect("x", `{"Id":"abc","RestartCount":2,"State":{"Running":true,"Status":"running","StartedAt":"2026-11-01T07:00:00.123456789Z"},"Config":{"Labels":{"a":"b"}}}`)
	if err != nil || !ok || c.RestartCount != 2 || c.Status != "running" || !c.Running ||
		!c.StartedAt.Equal(time.Date(2026, 11, 1, 7, 0, 0, 123456789, time.UTC)) {
		t.Fatalf("%+v %v %v", c, ok, err)
	}
	c, _, _ = parseInspect("x", `{"State":{"Status":"created","StartedAt":"0001-01-01T00:00:00Z"}}`)
	if !c.StartedAt.IsZero() {
		t.Fatalf("never-started container: %+v", c)
	}
}

func TestArgListOneShotAndLogOpts(t *testing.T) {
	s := RunSpec{Name: "n", Image: "img@sha256:1", OneShot: true, LogOpts: []string{"max-size=10m"}, RunArgs: []string{"-v", "/a:/a"}, Args: []string{"migrate"}}
	want := []string{"run", "--rm", "--name", "n", "--log-driver", "json-file", "--log-opt", "max-size=10m", "-v", "/a:/a", "img@sha256:1", "migrate"}
	if got := s.ArgList(); !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}
