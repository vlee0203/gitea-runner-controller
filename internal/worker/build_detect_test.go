package worker

import "testing"

func TestNeedsImageBuild(t *testing.T) {
	cases := []struct {
		name string
		wf   string
		job  string
		want bool
	}{
		{"docker run 命令", `
jobs:
  b:
    runs-on: ubuntu-latest
    steps:
      - run: docker build -t x .
      - run: docker push x
`, "b", true},
		{"官方 build action", `
jobs:
  b:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/build-push-action@v6
        with: { push: true }
`, "b", true},
		{"单独 docker login action 不触发构建", `
jobs:
  copy:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/login-action@v3
        with: { registry: registry.local, username: u, password: p }
      - run: docker pull registry.local/src:v1
      - run: docker tag registry.local/src:v1 registry.local/dst:v1
      - run: docker push registry.local/dst:v1
`, "copy", false},
		{"kaniko 写法", `
jobs:
  b:
    steps:
      - run: /kaniko/executor --context=dir://. --destination=x
`, "b", true},
		{"普通 job 不应命中", `
jobs:
  t:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
`, "t", false},
		{"纯 registry 搬运不需要 kaniko", `
jobs:
  copy:
    runs-on: ubuntu-latest
    steps:
      - run: docker login registry.local -u u -p p
      - run: docker pull registry.local/src:v1
      - run: docker tag registry.local/src:v1 registry.local/dst:v1
      - run: docker push registry.local/dst:v1
`, "copy", false},
		{"只构建另一个 job", `
jobs:
  no: { runs-on: ubuntu-latest, steps: [ { run: echo hi } ] }
  yes: { runs-on: ubuntu-latest, steps: [ { run: docker build -t x . } ] }
`, "no", false},
		{"job 名对不上", `
jobs:
  b: { runs-on: ubuntu-latest, steps: [ { run: docker build . } ] }
`, "other", false},
	}
	for _, c := range cases {
		if got := needsImageBuild([]byte(c.wf), c.job); got != c.want {
			t.Errorf("%s: needsImageBuild = %v, want %v", c.name, got, c.want)
		}
	}
}
