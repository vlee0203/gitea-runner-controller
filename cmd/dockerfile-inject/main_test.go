package main

import (
	"strings"
	"testing"
)

func TestInject(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		keys    []string
		want    string
		wantErr bool
	}{
		{
			name: "single stage",
			src:  "FROM golang:1.23\nRUN go mod download\n",
			keys: []string{"GOPROXY", "PIP_INDEX_URL"},
			want: "FROM golang:1.23\nARG GOPROXY\nARG PIP_INDEX_URL\nRUN go mod download\n",
		},
		{
			name: "multi stage",
			src:  "FROM golang:1.23 AS build\nRUN go build ./...\nFROM alpine\nCOPY --from=build /app /app\n",
			keys: []string{"GOPROXY"},
			want: "FROM golang:1.23 AS build\nARG GOPROXY\nRUN go build ./...\nFROM alpine\nARG GOPROXY\nCOPY --from=build /app /app\n",
		},
		{
			name: "comments and continuation",
			src:  "# syntax=docker/dockerfile:1\n# FROM fake\nFROM --platform=$BUILDPLATFORM golang:1.23 \\\n  AS build\nRUN true\n",
			keys: []string{"GOPROXY"},
			want: "# syntax=docker/dockerfile:1\n# FROM fake\nFROM --platform=$BUILDPLATFORM golang:1.23 \\\n  AS build\nARG GOPROXY\nRUN true\n",
		},
		{
			name: "heredoc keeps text out of the instruction stream",
			src:  "FROM alpine\nRUN <<EOF\necho FROM not-an-instruction\nEOF\nRUN true\n",
			keys: []string{"GOPROXY"},
			want: "FROM alpine\nARG GOPROXY\nRUN <<EOF\necho FROM not-an-instruction\nEOF\nRUN true\n",
		},
		{
			name: "CRLF is preserved",
			src:  "FROM alpine\r\nRUN true\r\n",
			keys: []string{"GOPROXY"},
			want: "FROM alpine\r\nARG GOPROXY\r\nRUN true\r\n",
		},
		{
			name: "existing ARG and local stage",
			src:  "FROM alpine AS base\nARG GOPROXY\nRUN true\nFROM base AS final\nRUN true\n",
			keys: []string{"GOPROXY"},
			want: "FROM alpine AS base\nARG GOPROXY\nARG GOPROXY\nRUN true\nFROM base AS final\nARG GOPROXY\nRUN true\n",
		},
		{
			name: "no trailing newline",
			src:  "FROM alpine",
			keys: []string{"GOPROXY"},
			want: "FROM alpine\nARG GOPROXY\n",
		},
		{
			name:    "no FROM",
			src:     "RUN true\n",
			keys:    []string{"GOPROXY"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inject([]byte(tt.src), tt.keys)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("inject: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestParseKeys(t *testing.T) {
	got, err := parseKeys(" GOPROXY, PIP_INDEX_URL,GOPROXY ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "GOPROXY,PIP_INDEX_URL" {
		t.Fatalf("keys = %v", got)
	}
	for _, bad := range []string{"", "1BAD", "A-B", "A B"} {
		if _, err := parseKeys(bad); err == nil {
			t.Fatalf("parseKeys(%q) succeeded", bad)
		}
	}
}
