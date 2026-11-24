package regcred

import (
	"reflect"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestDockerfileImages(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"plain", "FROM nginx:alpine\nCOPY . /usr/share/nginx/html\n", []string{"nginx:alpine"}},
		{"lowercase and platform", "from --platform=$BUILDPLATFORM ghcr.io/org/base:1 as build\n", []string{"ghcr.io/org/base:1"}},
		{"multi-stage", "FROM golang:1.23 AS build\nRUN go build\nFROM build AS test\nFROM gcr.io/distroless/static\nCOPY --from=build /app /app\n",
			[]string{"golang:1.23", "gcr.io/distroless/static"}},
		{"scratch", "FROM golang AS b\nFROM scratch\n", []string{"golang"}},
		{"ARG default", "ARG BASE=registry.lan:5000/py:3.12\nFROM ${BASE}\n", []string{"registry.lan:5000/py:3.12"}},
		{"ARG with tag var", "ARG REG=ghcr.io TAG=\"2\"\nFROM $REG/app:${TAG}\n", []string{"ghcr.io/app:2"}},
		{"ARG :- default", "FROM ${IMG:-myreg.io/x:1}\n", []string{"myreg.io/x:1"}},
		{"ARG without default", "ARG BASE\nFROM ${BASE}\n", nil},
		{"ARG after FROM doesn't apply", "FROM alpine\nARG LATER=x\nFROM ${LATER}\n", []string{"alpine"}},
		{"continuation and comments", "# syntax=docker/dockerfile:1\nFROM \\\n  quay.io/org/img:2 \\\n  AS base\n", []string{"quay.io/org/img:2"}},
	} {
		if got := DockerfileImages(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildImagesPicksDockerfiles(t *testing.T) {
	compose := "services:\n  web:\n    build:\n      context: ./web\n      dockerfile: ${DF:-prod.build}\n"
	files := []wire.StackFile{
		{Path: "Dockerfile", Content: []byte("FROM a.io/one\n")},
		{Path: "api/worker.Dockerfile", Content: []byte("FROM b.io/two\n")},
		{Path: "web/prod.build", Content: []byte("FROM c.io/three\n")},
		{Path: "Dockerfile.dev", Content: []byte("FROM d.io/four\n")},
		{Path: "notes.txt", Content: []byte("FROM e.io/nope\n")},
	}
	got := Hosts(BuildImages(compose, "", files))
	if want := []string{"a.io", "b.io", "c.io", "d.io"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
