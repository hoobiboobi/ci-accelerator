package history

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

// Every step of every recorded build points at a Dockerfile line holding
// its own instruction, in the Dockerfile the build recorded.
func TestDockerfileLinesMatchTheSteps(t *testing.T) {
	checked := 0
	for _, b := range loadFixtures(t) {
		name, text := b.Dockerfile()
		if b.prov == nil {
			continue
		}
		if name != "Dockerfile" || !strings.Contains(text, "FROM ") {
			t.Fatalf("%s: Dockerfile %q, %d bytes", b.Ref, name, len(text))
		}
		lines := strings.Split(text, "\n")
		for _, s := range b.Steps {
			if s.StepID == "" {
				continue
			}
			if len(s.Lines) == 0 || s.Line != s.Lines[0].Start {
				t.Errorf("%s %s: line %d, ranges %v", b.Ref, s.Key, s.Line, s.Lines)
				continue
			}
			got := strings.Fields(lines[s.Line-1])[0]
			want := strings.Fields(s.Name)[0]
			if !strings.EqualFold(got, want) {
				t.Errorf("%s %s %q: line %d is %q", b.Ref, s.Key, s.Name, s.Line, lines[s.Line-1])
			}
			checked++
		}
	}
	if checked < 50 {
		t.Fatalf("checked %d steps, the fixtures have more", checked)
	}
}

// A RUN continued over several lines is one range. Only locations in the
// Dockerfile count when the provenance records several sources.
func TestLineRangesOfTheDockerfileSource(t *testing.T) {
	dockerfile := "FROM alpine AS base\nRUN apk add curl \\\n    && rm -rf /var/cache/apk\n"
	prov := `{"buildDefinition":{"internalParameters":{"buildConfig":{
		"llbDefinition":[{"id":"step0","op":{"Op":{"exec":{"meta":{"args":["sh"]}}}}}],
		"digestMapping":{"sha256:aa":"step0"}}}},
	"runDetails":{"metadata":{"buildkit_metadata":{"source":{
		"infos":[{"filename":"other.txt","data":"eA=="},
		         {"filename":"Dockerfile","language":"Dockerfile","data":"` + base64.StdEncoding.EncodeToString([]byte(dockerfile)) + `"}],
		"locations":{"step0":{"locations":[
			{"ranges":[{"start":{"line":1},"end":{"line":1}}]},
			{"sourceIndex":1,"ranges":[{"start":{"line":2},"end":{"line":3}}]}]}}}}}}}`
	p, err := ParseProvenance([]byte(prov), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Dockerfile != dockerfile || p.DockerfileName != "Dockerfile" {
		t.Errorf("Dockerfile %q %q", p.DockerfileName, p.Dockerfile)
	}
	if want := []LineRange{{Start: 2, End: 3}}; !reflect.DeepEqual(p.Ranges["step0"], want) || p.Lines["step0"] != 2 {
		t.Errorf("ranges %v, first line %d; want %v, 2", p.Ranges["step0"], p.Lines["step0"], want)
	}
}
