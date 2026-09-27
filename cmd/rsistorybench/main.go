// rsistorybench compares independently captured agent traces on a frozen
// user-story suite. Exit 0 means eligible, 1 means a valid but ineligible
// candidate, and 2 means malformed evidence or invocation.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/storybench"
)

func main() { os.Exit(run()) }

func run() int {
	suitePath := flag.String("suite", "", "frozen story suite JSON")
	baselinePath := flag.String("baseline", "", "harness-captured accepted-bundle trace JSON")
	candidatePath := flag.String("candidate", "", "harness-captured candidate-bundle trace JSON")
	improver := flag.String("improver", "", "candidate-producing agent identity")
	outPath := flag.String("out", "", "optional result JSON path (stdout by default)")
	evidencePath := flag.String("evidence-out", "", "optional baseline/candidate trace evidence JSON path")
	flag.Parse()
	if *suitePath == "" || *baselinePath == "" || *candidatePath == "" || *improver == "" {
		fmt.Fprintln(os.Stderr, "required: --suite --baseline --candidate --improver")
		return 2
	}
	rawSuite, err := os.ReadFile(*suitePath)
	if err != nil {
		return fail(err)
	}
	baseline, err := readRun(*baselinePath)
	if err != nil {
		return fail(fmt.Errorf("baseline: %w", err))
	}
	candidate, err := readRun(*candidatePath)
	if err != nil {
		return fail(fmt.Errorf("candidate: %w", err))
	}
	result, err := storybench.Evaluate(rawSuite, baseline, candidate, *improver)
	if err != nil {
		return fail(err)
	}
	if *evidencePath != "" {
		rawEvidence, err := json.MarshalIndent(storybench.Evidence{Baseline: baseline, Candidate: candidate}, "", "  ")
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(*evidencePath, append(rawEvidence, '\n'), 0o600); err != nil {
			return fail(err)
		}
	}
	rawResult, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fail(err)
	}
	rawResult = append(rawResult, '\n')
	if *outPath != "" {
		if err := os.WriteFile(*outPath, rawResult, 0o600); err != nil {
			return fail(err)
		}
	} else if _, err := os.Stdout.Write(rawResult); err != nil {
		return fail(err)
	}
	if !result.Eligible {
		return 1
	}
	return 0
}

func readRun(path string) (storybench.Run, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return storybench.Run{}, err
	}
	var run storybench.Run
	if err := json.Unmarshal(raw, &run); err != nil {
		return storybench.Run{}, err
	}
	return run, nil
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, err)
	return 2
}
