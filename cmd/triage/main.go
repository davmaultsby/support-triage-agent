// Command triage runs the support agent on one ticket, or on the eval set.
//
//	triage eval [-model claude-opus-5-5] [-effort low] [-j 4] [-only t01,t02]
//	triage ask -from ava.chen@example.com [-subject "..."] "message text"
//
// -model is a Claude model id, ollama:<model> for a local model, or baseline.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/davmaultsby/support-triage-agent/agent"
	"github.com/davmaultsby/support-triage-agent/eval"
	"github.com/davmaultsby/support-triage-agent/orders"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 || (os.Args[1] != "eval" && os.Args[1] != "ask") {
		log.Fatal("usage: triage eval|ask [flags]; see triage eval -h")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	model := fs.String("model", "claude-opus-5-5", "Claude model id, ollama:<model>, or baseline")
	effort := fs.String("effort", "", "Claude effort: low, medium, high, xhigh or max (default: the model's)")
	ollamaURL := fs.String("ollama", "http://localhost:11434", "Ollama server for ollama:<model>")
	jobs := fs.Int("j", 4, "eval: tickets in flight at once")
	tickets := fs.String("tickets", "testdata/tickets.jsonl", "eval: ticket file")
	only := fs.String("only", "", "eval: comma-separated ticket ids to run")
	out := fs.String("out", "runs", "eval: directory for the full results (JSON)")
	from := fs.String("from", "", "ask: sender email address")
	subject := fs.String("subject", "", "ask: email subject")
	fs.Parse(os.Args[2:])

	ctx := context.Background()
	db, err := orders.Open(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	// The tools talk to the orders API over HTTP, as they would in production.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go http.Serve(ln, orders.Handler(db))
	api := &agent.OrdersAPI{URL: "http://" + ln.Addr().String()}

	handle := handler(*model, *effort, *ollamaURL, api)

	if cmd == "ask" {
		if *from == "" || fs.NArg() == 0 {
			log.Fatal(`usage: triage ask -from <email> [-subject "..."] "message text"`)
		}
		d, err := handle(ctx, agent.Ticket{ID: "ask", From: *from, Subject: *subject, Body: strings.Join(fs.Args(), " ")})
		if err != nil {
			log.Fatal(err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(d)
		return
	}

	cases, err := eval.Load(*tickets)
	if err != nil {
		log.Fatal(err)
	}
	if *only != "" {
		ids := strings.Split(*only, ",")
		cases = slices.DeleteFunc(cases, func(c eval.Case) bool { return !slices.Contains(ids, c.ID) })
	}
	// Start from the seeded state: no refund requests.
	if _, err := db.Exec(ctx, `TRUNCATE refund_requests`); err != nil {
		log.Fatal(err)
	}

	baseResults := eval.Run(ctx, cases, *jobs, handler("baseline", "", "", api))
	baseline := eval.Summarize("baseline", baseResults)
	name := modelName(*model, *effort)
	start := time.Now()
	results, s := baseResults, baseline
	if *model != "baseline" {
		results = eval.Run(ctx, cases, *jobs, handle)
		s = eval.Summarize(name, results)
	}
	fmt.Printf("evaluated %d/%d tickets, %d harness errors, in %s\n\n", s.Tickets-s.Errors, s.Tickets, s.Errors, time.Since(start).Round(time.Second))
	scorecard(os.Stdout, baseline, s, results)

	if err := os.MkdirAll(*out, 0o755); err == nil {
		path := filepath.Join(*out, fmt.Sprintf("%s-%s.json", time.Now().Format("20060102-150405"), strings.NewReplacer(":", "_", "/", "_").Replace(name)))
		b, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(path, b, 0o644); err == nil {
			fmt.Printf("\nfull results: %s\n", path)
		}
	}
}

func modelName(model, effort string) string {
	if effort != "" && !strings.HasPrefix(model, "ollama:") && model != "baseline" {
		return model + "@" + effort
	}
	return model
}

func handler(model, effort, ollamaURL string, api *agent.OrdersAPI) func(context.Context, agent.Ticket) (agent.Decision, error) {
	if model == "baseline" {
		return func(ctx context.Context, t agent.Ticket) (agent.Decision, error) { return agent.Baseline(ctx, api, t) }
	}
	var m agent.Model
	if name, ok := strings.CutPrefix(model, "ollama:"); ok {
		m = &agent.Ollama{URL: ollamaURL, Model: name}
	} else {
		m = agent.NewClaude(model, effort)
	}
	return func(ctx context.Context, t agent.Ticket) (agent.Decision, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return agent.Run(ctx, m, api, t)
	}
}

func scorecard(w io.Writer, base, s eval.Summary, results []eval.Result) {
	pct := func(n, d int) string { return fmt.Sprintf("%d/%d (%.0f%%)", n, d, 100*float64(n)/float64(max(d, 1))) }
	row := func(label, a, b string) { fmt.Fprintf(w, "| %s | %s | %s |\n", label, a, b) }
	money := func(x eval.Summary) string {
		if x.CostUSD < 0 {
			return "unknown"
		}
		return fmt.Sprintf("$%.2f ($%.3f per ticket)", x.CostUSD, x.CostUSD/float64(max(x.Tickets, 1)))
	}
	tokens := func(x eval.Summary) string {
		u := x.Usage
		if u.InputTokens+u.OutputTokens == 0 {
			return "-"
		}
		return fmt.Sprintf("%dk in, %dk out, %dk cache read", (u.InputTokens+u.CacheWriteTokens)/1000, u.OutputTokens/1000, u.CacheReadTokens/1000)
	}

	row("", "baseline", s.Model)
	row("---", "---", "---")
	row("passed", pct(base.Passed, base.Tickets), pct(s.Passed, s.Tickets))
	row("resolved without a person", pct(base.AutoResolved, base.Tickets), pct(s.AutoResolved, s.Tickets))
	row("escalated", pct(base.Escalated, base.Tickets), pct(s.Escalated, s.Tickets))
	row("safety violations", fmt.Sprint(base.Violations), fmt.Sprint(s.Violations))
	row("harness errors", fmt.Sprint(base.Errors), fmt.Sprint(s.Errors))
	cats := make([]string, 0, len(s.ByCategory))
	for c := range s.ByCategory {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		b, m := base.ByCategory[c], s.ByCategory[c]
		row("  "+c, fmt.Sprintf("%d/%d", b[0], b[1]), fmt.Sprintf("%d/%d", m[0], m[1]))
	}
	row("mean time per ticket", base.MeanLatency.Round(time.Millisecond).String(), s.MeanLatency.Round(time.Millisecond).String())
	row("tokens", tokens(base), tokens(s))
	row("cost", money(base), money(s))
	keep, why := eval.Verdict(s, base)
	verdict := "RETIRE"
	if keep {
		verdict = "KEEP"
	}
	fmt.Fprintf(w, "\nverdict: %s (%s)\n", verdict, why)

	var failed []eval.Result
	for _, r := range results {
		if !r.Pass {
			failed = append(failed, r)
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(w, "\nnot passed:\n")
		for _, r := range failed {
			problems := append(append([]string{}, r.Violations...), r.Failures...)
			if r.Error != "" {
				problems = []string{"error: " + r.Error}
			}
			fmt.Fprintf(w, "  %s (%s): %s\n", r.ID, r.Category, strings.Join(problems, "; "))
		}
	}
}
