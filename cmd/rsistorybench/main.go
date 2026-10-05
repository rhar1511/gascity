// rsistorybench compares independently captured agent traces on a frozen
// user-story suite. Exit 0 means eligible, 1 means a valid but ineligible
// candidate, and 2 means malformed evidence or invocation.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storybench"
	"github.com/spf13/cobra"
)

func main() { os.Exit(run()) }

type benchmarkOptions struct {
	suite, baseline, candidate, improver, out, evidence string
}

func run() int {
	var opts benchmarkOptions
	exitCode := 0
	command := &cobra.Command{
		Use: "rsistorybench", Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
		Run: func(*cobra.Command, []string) { exitCode = runBenchmark(opts) },
	}
	command.Flags().StringVar(&opts.suite, "suite", "", "frozen story suite JSON")
	command.Flags().StringVar(&opts.baseline, "baseline", "", "harness-captured accepted-bundle trace JSON")
	command.Flags().StringVar(&opts.candidate, "candidate", "", "harness-captured candidate-bundle trace JSON")
	command.Flags().StringVar(&opts.improver, "improver", "", "candidate-producing agent identity")
	command.Flags().StringVar(&opts.out, "out", "", "optional result JSON path (stdout by default)")
	command.Flags().StringVar(&opts.evidence, "evidence-out", "", "optional baseline/candidate trace evidence JSON path")
	command.SetOut(os.Stdout)
	command.SetErr(os.Stderr)
	if err := command.Execute(); err != nil {
		return fail(err)
	}
	return exitCode
}

func runBenchmark(opts benchmarkOptions) int {
	if opts.suite == "" || opts.baseline == "" || opts.candidate == "" || opts.improver == "" {
		fmt.Fprintln(os.Stderr, "required: --suite --baseline --candidate --improver")
		return 2
	}
	rawSuite, err := os.ReadFile(opts.suite)
	if err != nil {
		return fail(err)
	}
	baseline, err := readRun(opts.baseline)
	if err != nil {
		return fail(fmt.Errorf("baseline: %w", err))
	}
	candidate, err := readRun(opts.candidate)
	if err != nil {
		return fail(fmt.Errorf("candidate: %w", err))
	}
	result, err := storybench.Evaluate(rawSuite, baseline, candidate, opts.improver)
	if err != nil {
		return fail(err)
	}
	if opts.evidence != "" {
		rawEvidence, err := json.MarshalIndent(storybench.Evidence{Baseline: baseline, Candidate: candidate}, "", "  ")
		if err != nil {
			return fail(err)
		}
		if err := fsys.WriteFileAtomic(fsys.OSFS{}, opts.evidence, append(rawEvidence, '\n'), 0o600); err != nil {
			return fail(err)
		}
	}
	rawResult, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fail(err)
	}
	rawResult = append(rawResult, '\n')
	if opts.out != "" {
		if err := fsys.WriteFileAtomic(fsys.OSFS{}, opts.out, rawResult, 0o600); err != nil {
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
