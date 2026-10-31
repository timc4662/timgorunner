// timgorunner runs `go test -json` for the given packages, shows progress live
// on the console, and writes a self-contained HTML report to disk.
//
// Usage:
//
//	timgorunner ./... --output=results.html
//	timgorunner ./... --output=out/report.html -- -race -count=1 -run TestFoo
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const usage = `timgorunner - run go tests, watch them live, get an HTML report

Usage:
  timgorunner [packages...] [--output=results.html] [--no-color] [-- go-test-flags...]

Examples:
  timgorunner ./... --output=results.html
  timgorunner ./internal/... -race -count=1
  timgorunner ./... --output=report.html -- -run TestFoo -timeout 30s

Options:
  --output, -o FILE   HTML report path (default results.html)
  --no-color          disable ANSI colours on the console
  --                  everything after is passed to 'go test' unchanged
  -h, --help          show this help

Any other -flag (e.g. -race, -count=1) is passed through to 'go test'.
Common value flags (-run X, -timeout 30s, -tags x ...) work too; for anything else use '--'.
The exit code matches 'go test' (non-zero on failures), or 2 on tool errors.
`

// ---------- config ----------

type config struct {
	output    string
	color     bool
	patterns  []string
	passthru  []string
	showUsage bool
}

func useColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var valueFlags = map[string]bool{
	"run": true, "timeout": true, "tags": true, "count": true, "p": true,
	"parallel": true, "cpu": true, "skip": true, "shuffle": true, "coverprofile": true,
	"covermode": true, "bench": true, "benchtime": true, "ldflags": true, "gcflags": true,
}

func parseArgs(args []string) (config, error) {
	c := config{output: "results.html", color: useColor()}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			c.passthru = append(c.passthru, args[i+1:]...)
			i = len(args)
		case a == "-h" || a == "--help" || a == "-help":
			c.showUsage = true
		case a == "--no-color":
			c.color = false
		case a == "--output" || a == "-output" || a == "-o":
			if i+1 >= len(args) {
				return c, errors.New("--output requires a value")
			}
			i++
			c.output = args[i]
		case strings.HasPrefix(a, "--output="):
			c.output = strings.TrimPrefix(a, "--output=")
		case strings.HasPrefix(a, "-output="):
			c.output = strings.TrimPrefix(a, "-output=")
		case strings.HasPrefix(a, "-"):
			c.passthru = append(c.passthru, a)
			// Common go test flags that take a separate value.
			if !strings.Contains(a, "=") && valueFlags[strings.TrimLeft(a, "-")] && i+1 < len(args) {
				i++
				c.passthru = append(c.passthru, args[i])
			}
		default:
			c.patterns = append(c.patterns, a)
		}
	}
	if c.output == "" {
		return c, errors.New("--output must not be empty")
	}
	if len(c.patterns) == 0 {
		c.patterns = []string{"./..."}
	}
	return c, nil
}

// ---------- data model ----------

// event is one line of `go test -json` (test2json) output.
type event struct {
	Time       time.Time `json:"Time"`
	Action     string    `json:"Action"`
	Package    string    `json:"Package"`
	ImportPath string    `json:"ImportPath"`
	Test       string    `json:"Test"`
	Output     string    `json:"Output"`
	Elapsed    float64   `json:"Elapsed"`
}

type testResult struct {
	Name    string
	Status  string // running, pass, fail, skip
	Elapsed float64
	Depth   int
	Output  string
	out     []string
}

type pkgResult struct {
	Name    string
	Status  string // pass, fail, skip
	Elapsed float64
	Cached  bool
	NoTests bool
	Tests   []*testResult
	Output  string
	out     []string
	idx     map[string]*testResult
}

func (p *pkgResult) test(name string) *testResult {
	if t, ok := p.idx[name]; ok {
		return t
	}
	t := &testResult{Name: name, Depth: strings.Count(name, "/")}
	p.idx[name] = t
	p.Tests = append(p.Tests, t)
	return t
}

type report struct {
	Title    string
	Command  string
	GoVer    string
	Started  string
	Duration string
	OK       bool

	Passed, Failed, Skipped, Total int
	PkgPass, PkgFail, PkgNone      int

	Packages []*pkgResult
	Extra    string // non-JSON output (build errors, stderr)
}

// ---------- collector / console ----------

type collector struct {
	cfg   config
	pkgs  map[string]*pkgResult
	extra []string
	mu    sync.Mutex
}

func (c *collector) paint(code, s string) string {
	if !c.cfg.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (c *collector) pkg(name string) *pkgResult {
	if p, ok := c.pkgs[name]; ok {
		return p
	}
	p := &pkgResult{Name: name, idx: map[string]*testResult{}}
	c.pkgs[name] = p
	return p
}

// keepOutput drops the "=== RUN" / "--- PASS" bookkeeping lines test2json
// emits as output; the status badge already conveys them.
func keepOutput(s string) bool {
	t := strings.TrimLeft(s, " \t")
	return !strings.HasPrefix(t, "=== ") && !strings.HasPrefix(t, "--- ")
}

func (c *collector) label(status string) string {
	switch status {
	case "pass":
		return c.paint("32;1", "PASS")
	case "fail":
		return c.paint("31;1", "FAIL")
	case "skip":
		return c.paint("33", "SKIP")
	default:
		return c.paint("2", "RUN ")
	}
}

func (c *collector) printLines(lines []string, code string) {
	for _, l := range lines {
		if !keepOutput(l) {
			continue
		}
		fmt.Print("        " + c.paint(code, strings.TrimRight(l, "\r\n")) + "\n")
	}
}

func (c *collector) handle(ev event) {
	if ev.Action == "build-output" {
		fmt.Fprint(os.Stderr, ev.Output)
		c.extra = append(c.extra, ev.Output)
		return
	}
	if ev.Package == "" {
		return
	}
	p := c.pkg(ev.Package)

	if ev.Test == "" { // package-level event
		switch ev.Action {
		case "output":
			p.out = append(p.out, ev.Output)
			if strings.Contains(ev.Output, "(cached)") {
				p.Cached = true
			}
			if strings.Contains(ev.Output, "[no test files]") {
				p.NoTests = true
			}
		case "pass", "fail", "skip":
			p.Status = ev.Action
			p.Elapsed = ev.Elapsed
			c.printPkg(p)
		}
		return
	}

	t := p.test(ev.Test)
	switch ev.Action {
	case "run":
		t.Status = "running"
		fmt.Printf("  %s  %s  %s\n", c.label("run"), ev.Test, c.paint("2", p.Name))
	case "output":
		t.out = append(t.out, ev.Output)
	case "pass", "fail", "skip":
		t.Status = ev.Action
		t.Elapsed = ev.Elapsed
		fmt.Printf("  %s  %s  %s  %s\n", c.label(ev.Action), ev.Test,
			c.paint("2", fmt.Sprintf("(%.2fs)", ev.Elapsed)), c.paint("2", p.Name))
		if ev.Action == "fail" {
			c.printLines(t.out, "31")
		}
	}
}

func (c *collector) printPkg(p *pkgResult) {
	if p.NoTests {
		fmt.Printf("  %s  %s  %s\n", c.paint("2", "----"), c.paint("2", p.Name), c.paint("2", "[no test files]"))
		return
	}
	note := ""
	if p.Cached {
		note = " (cached)"
	}
	fmt.Printf("%s %s  %s\n", c.paint("1", "PKG "+strings.ToUpper(p.Status)), p.Name,
		c.paint("2", fmt.Sprintf("(%.2fs)%s", p.Elapsed, note)))
	// Failed with no failing test => build error, panic, timeout, etc. Show why.
	if p.Status == "fail" {
		anyFailed := false
		for _, t := range p.Tests {
			if t.Status == "fail" {
				anyFailed = true
				break
			}
		}
		if !anyFailed {
			c.printLines(p.out, "31")
		}
	}
}

// finish resolves unfinished work and computes the report.
func (c *collector) finish(cmdline, goVer string, started time.Time, wall time.Duration) *report {
	r := &report{
		Title:    "Go test results",
		Command:  cmdline,
		GoVer:    goVer,
		Started:  started.Format("Mon 2 Jan 2006 15:04:05 MST"),
		Duration: wall.Round(10 * time.Millisecond).String(),
	}
	for _, p := range c.pkgs {
		for _, t := range p.Tests {
			if t.Status == "running" || t.Status == "" {
				t.Status = "fail"
				t.out = append(t.out, "(test did not finish: interrupted, timed out or panicked)\n")
			}
			t.Output = joinOutput(t.out)
			switch t.Status {
			case "pass":
				r.Passed++
			case "fail":
				r.Failed++
			case "skip":
				r.Skipped++
			}
		}
		if p.Status == "" {
			p.Status = "fail"
			p.out = append(p.out, "(package did not report a result)\n")
		}
		p.Output = joinOutput(p.out)
		switch {
		case p.NoTests:
			r.PkgNone++
		case p.Status == "fail":
			r.PkgFail++
		default:
			r.PkgPass++
		}
		r.Packages = append(r.Packages, p)
	}
	sort.Slice(r.Packages, func(i, j int) bool { return r.Packages[i].Name < r.Packages[j].Name })
	r.Total = r.Passed + r.Failed + r.Skipped
	r.OK = r.Failed == 0 && r.PkgFail == 0
	r.Extra = strings.TrimSpace(strings.Join(c.extra, ""))
	return r
}

func joinOutput(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		if keepOutput(l) {
			b.WriteString(l)
		}
	}
	return strings.TrimRight(b.String(), "\r\n ")
}

// ---------- main ----------

func main() { os.Exit(run()) }

func run() int {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "timgorunner:", err)
		fmt.Fprint(os.Stderr, "\n"+usage)
		return 2
	}
	if cfg.showUsage {
		fmt.Print(usage)
		return 0
	}

	goArgs := append([]string{"test", "-json"}, cfg.passthru...)
	goArgs = append(goArgs, cfg.patterns...)
	cmdline := "go " + strings.Join(goArgs, " ")

	cmd := exec.Command("go", goArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "timgorunner:", err)
		return 2
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "timgorunner:", err)
		return 2
	}

	// Ctrl-C reaches the child via the terminal's process group. We swallow it
	// here so we still get to write a report for whatever completed.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	go func() {
		for range sigs {
		}
	}()

	col := &collector{cfg: cfg, pkgs: map[string]*pkgResult{}}
	fmt.Println(col.paint("1", "$ "+cmdline))
	fmt.Println()

	started := time.Now()
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "timgorunner: could not start go:", err)
		return 2
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // stderr: echo live and keep for the report
		defer wg.Done()
		br := bufio.NewReader(stderr)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				fmt.Fprint(os.Stderr, line)
				col.mu.Lock()
				col.extra = append(col.extra, line)
				col.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	br := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var ev event
			if jerr := json.Unmarshal(line, &ev); jerr != nil || ev.Action == "" {
				// Not test2json: show it and keep it for the report.
				fmt.Print(string(line))
				col.mu.Lock()
				col.extra = append(col.extra, string(line))
				col.mu.Unlock()
			} else {
				col.mu.Lock()
				col.handle(ev)
				col.mu.Unlock()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				fmt.Fprintln(os.Stderr, "timgorunner: reading output:", rerr)
			}
			break
		}
	}
	wg.Wait()
	waitErr := cmd.Wait()
	wall := time.Since(started)
	signal.Stop(sigs)

	exit := 0
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		exit = ee.ExitCode()
		if exit <= 0 {
			exit = 1
		}
	} else if waitErr != nil {
		fmt.Fprintln(os.Stderr, "timgorunner:", waitErr)
		exit = 2
	}

	rep := col.finish(cmdline, goVersion(), started, wall)
	if exit != 0 && rep.OK {
		rep.OK = false // go test failed for a reason we did not see as a test failure
	}

	printSummary(col, rep)

	if werr := writeHTML(cfg.output, rep); werr != nil {
		fmt.Fprintln(os.Stderr, "timgorunner: writing report:", werr)
		if exit == 0 {
			exit = 2
		}
	} else {
		abs, _ := filepath.Abs(cfg.output)
		fmt.Printf("HTML report: %s\n", abs)
	}
	return exit
}

func goVersion() string {
	out, err := exec.Command("go", "version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func printSummary(c *collector, r *report) {
	fmt.Println()
	fmt.Println(c.paint("1", "==================== Summary ===================="))
	fmt.Printf("  Tests:     %d total, %s, %s, %s\n", r.Total,
		c.paint("32", fmt.Sprintf("%d passed", r.Passed)),
		c.paint("31", fmt.Sprintf("%d failed", r.Failed)),
		c.paint("33", fmt.Sprintf("%d skipped", r.Skipped)))
	fmt.Printf("  Packages:  %d passed, %d failed, %d without tests\n", r.PkgPass, r.PkgFail, r.PkgNone)
	fmt.Printf("  Duration:  %s\n", r.Duration)
	if r.Failed > 0 {
		fmt.Println("  Failed tests:")
		for _, p := range r.Packages {
			for _, t := range p.Tests {
				if t.Status == "fail" {
					fmt.Printf("    - %s  %s\n", t.Name, c.paint("2", p.Name))
				}
			}
		}
	}
	if r.OK {
		fmt.Println("  Result:    " + c.paint("32;1", "PASS"))
	} else {
		fmt.Println("  Result:    " + c.paint("31;1", "FAIL"))
	}
}

// ---------- HTML ----------

func writeHTML(path string, r *report) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tmpl.Execute(f, r); err != nil {
		return err
	}
	return f.Close()
}

var tmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"secs":   func(f float64) string { return fmt.Sprintf("%.2fs", f) },
	"indent": func(d int) int { return d * 18 },
	"upper":  strings.ToUpper,
}).Parse(htmlTemplate))

const htmlTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{--bg:#fff;--fg:#1f2328;--mut:#656d76;--card:#f6f8fa;--bd:#d0d7de;--pass:#1a7f37;--fail:#cf222e;--skip:#9a6700;--passbg:#dafbe1;--failbg:#ffebe9;--skipbg:#fff8c5}
@media(prefers-color-scheme:dark){:root{--bg:#0d1117;--fg:#e6edf3;--mut:#8b949e;--card:#161b22;--bd:#30363d;--pass:#3fb950;--fail:#f85149;--skip:#d29922;--passbg:#12261e;--failbg:#2d1215;--skipbg:#2b2110}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
main{max-width:1000px;margin:0 auto;padding:24px 16px 64px}
h1{font-size:22px;margin:0 0 4px}
.meta{color:var(--mut);font-size:12px;margin-bottom:16px;word-break:break-word}
.banner{display:flex;align-items:center;gap:12px;padding:14px 16px;border-radius:8px;border:1px solid var(--bd);margin-bottom:16px;font-weight:600;font-size:18px}
.banner.ok{background:var(--passbg);color:var(--pass);border-color:var(--pass)}
.banner.bad{background:var(--failbg);color:var(--fail);border-color:var(--fail)}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(130px,1fr));gap:10px;margin-bottom:16px}
.card{background:var(--card);border:1px solid var(--bd);border-radius:8px;padding:10px 12px}
.card b{display:block;font-size:22px}
.card span{color:var(--mut);font-size:12px}
.card.pass b{color:var(--pass)}.card.fail b{color:var(--fail)}.card.skip b{color:var(--skip)}
.bar{display:flex;gap:8px;flex-wrap:wrap;margin-bottom:14px}
button{background:var(--card);color:var(--fg);border:1px solid var(--bd);border-radius:6px;padding:5px 12px;font:inherit;cursor:pointer}
button.active{border-color:var(--fg);font-weight:600}
.pkg{border:1px solid var(--bd);border-radius:8px;margin-bottom:12px;overflow:hidden}
.pkg>summary{cursor:pointer;padding:10px 12px;background:var(--card);display:flex;gap:10px;align-items:center;flex-wrap:wrap}
.pkg>summary .name{font-weight:600;word-break:break-all}
.badge{display:inline-block;min-width:44px;text-align:center;font-size:11px;font-weight:700;padding:1px 6px;border-radius:10px;border:1px solid}
.badge.pass{color:var(--pass);background:var(--passbg);border-color:var(--pass)}
.badge.fail{color:var(--fail);background:var(--failbg);border-color:var(--fail)}
.badge.skip{color:var(--skip);background:var(--skipbg);border-color:var(--skip)}
.dim{color:var(--mut);font-size:12px}
.test{border-top:1px solid var(--bd)}
.test>summary{padding:6px 12px;cursor:pointer;display:flex;gap:10px;align-items:center}
.test.noout>summary{cursor:default;list-style:none}
.test.noout>summary::-webkit-details-marker{display:none}
.test .tname{flex:1;word-break:break-all;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px}
pre{margin:0;padding:10px 12px;background:var(--card);border-top:1px solid var(--bd);overflow:auto;font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;white-space:pre-wrap;word-break:break-word}
.hidden{display:none!important}
h2{font-size:16px;margin:24px 0 8px}
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
<div class="meta">{{.Started}} &middot; {{.Duration}}{{if .GoVer}} &middot; {{.GoVer}}{{end}}<br><code>{{.Command}}</code></div>

<div class="banner {{if .OK}}ok{{else}}bad{{end}}">{{if .OK}}PASS &mdash; all tests passed{{else}}FAIL &mdash; {{.Failed}} failed test(s), {{.PkgFail}} failed package(s){{end}}</div>

<div class="cards">
<div class="card"><b>{{.Total}}</b><span>tests</span></div>
<div class="card pass"><b>{{.Passed}}</b><span>passed</span></div>
<div class="card fail"><b>{{.Failed}}</b><span>failed</span></div>
<div class="card skip"><b>{{.Skipped}}</b><span>skipped</span></div>
<div class="card"><b>{{.PkgPass}}</b><span>packages passed</span></div>
<div class="card"><b>{{.Duration}}</b><span>duration</span></div>
</div>

{{if .Extra}}<h2>Tool / build output</h2><pre>{{.Extra}}</pre>{{end}}

<div class="bar">
<button class="active" data-f="all">All</button>
<button data-f="fail">Failed</button>
<button data-f="skip">Skipped</button>
<button data-f="pass">Passed</button>
<span style="flex:1"></span>
<button id="expand">Expand all</button>
<button id="collapse">Collapse all</button>
</div>

{{range .Packages}}
<details class="pkg" data-status="{{.Status}}" {{if or (eq .Status "fail") (not .NoTests)}}open{{end}}>
<summary>
<span class="badge {{if .NoTests}}skip{{else}}{{.Status}}{{end}}">{{if .NoTests}}NONE{{else}}{{upper .Status}}{{end}}</span>
<span class="name">{{.Name}}</span>
<span class="dim">{{if .NoTests}}no test files{{else}}{{secs .Elapsed}}{{if .Cached}} &middot; cached{{end}}{{end}}</span>
</summary>
{{range .Tests}}
<details class="test {{.Status}}{{if not .Output}} noout{{end}}" data-status="{{.Status}}" {{if and (eq .Status "fail") .Output}}open{{end}}>
<summary style="padding-left:{{indent .Depth | printf "%d"}}px">
<span class="badge {{.Status}}">{{upper .Status}}</span>
<span class="tname">{{.Name}}</span>
<span class="dim">{{secs .Elapsed}}</span>
</summary>
{{if .Output}}<pre>{{.Output}}</pre>{{end}}
</details>
{{end}}
{{if and (eq .Status "fail") .Output}}<pre>{{.Output}}</pre>{{end}}
</details>
{{end}}
</main>
<script>
(function(){
  var buttons=document.querySelectorAll('button[data-f]');
  function apply(f){
    buttons.forEach(function(b){b.classList.toggle('active',b.dataset.f===f)});
    document.querySelectorAll('.test').forEach(function(t){
      t.classList.toggle('hidden',f!=='all'&&t.dataset.status!==f);
    });
    document.querySelectorAll('.pkg').forEach(function(p){
      var vis=p.querySelectorAll('.test:not(.hidden)').length>0;
      var show=f==='all'||vis||(f==='fail'&&p.dataset.status==='fail');
      p.classList.toggle('hidden',!show);
    });
  }
  buttons.forEach(function(b){b.addEventListener('click',function(){apply(b.dataset.f)})});
  document.getElementById('expand').addEventListener('click',function(){
    document.querySelectorAll('details').forEach(function(d){d.open=true});
  });
  document.getElementById('collapse').addEventListener('click',function(){
    document.querySelectorAll('details.test').forEach(function(d){d.open=false});
  });
})();
</script>
</body>
</html>
`
