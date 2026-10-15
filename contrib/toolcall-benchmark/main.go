// toolcallbench evaluates pre-call proposals without executing any of them.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/toolcallbench"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("toolcallbench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	suite := flags.String("suite", "", "evaluator-owned frozen suite JSON")
	baseline := flags.String("baseline", "", "independently captured baseline proposals JSON")
	candidate := flags.String("candidate", "", "independently captured candidate proposals JSON")
	improver := flags.String("improver", "", "candidate-producing identity")
	out := flags.String("out", "", "optional private atomic JSON output file")
	contexts := flags.Bool("contexts", false, "export candidate context without oracle answers")
	fail := func(err error) int {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 2
		}
		return 2
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("unexpected positional arguments"))
	}
	if *suite == "" {
		return fail(fmt.Errorf("required: --suite"))
	}
	if *contexts && (*baseline != "" || *candidate != "" || *improver != "") {
		return fail(fmt.Errorf("contexts mode accepts only --suite and optional --out"))
	}
	if !*contexts && (*baseline == "" || *candidate == "" || *improver == "") {
		return fail(fmt.Errorf("required: --suite --baseline --candidate --improver"))
	}
	rawSuite, err := readInput(*suite)
	if err != nil {
		return fail(fmt.Errorf("suite: %w", err))
	}
	var output []byte
	if *contexts {
		output, err = toolcallbench.Contexts(rawSuite)
	} else {
		base, readErr := readInput(*baseline)
		if readErr != nil {
			return fail(fmt.Errorf("baseline: %w", readErr))
		}
		cand, readErr := readInput(*candidate)
		if readErr != nil {
			return fail(fmt.Errorf("candidate: %w", readErr))
		}
		report, evalErr := toolcallbench.Evaluate(rawSuite, base, cand, *improver)
		if evalErr != nil {
			return fail(evalErr)
		}
		output, err = json.MarshalIndent(report, "", "  ")
	}
	if err != nil {
		return fail(err)
	}
	output = append(output, '\n')
	if *out != "" {
		if err := refuseInputOverwrite(*out, []string{*suite, *baseline, *candidate}); err != nil {
			return fail(err)
		}
		err = fsys.WriteFileAtomic(fsys.OSFS{}, *out, output, 0o600)
	} else {
		_, err = stdout.Write(output)
	}
	if err != nil {
		return fail(err)
	}
	// A valid report, even with regressions, is not promotion eligibility.
	return 0
}

func readInput(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(raw) > 8<<20 {
		return nil, fmt.Errorf("input exceeds 8 MiB")
	}
	return raw, nil
}

func refuseInputOverwrite(output string, inputs []string) error {
	outPath, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	outInfo, err := os.Stat(output)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, input := range inputs {
		if input == "" {
			continue
		}
		inputPath, err := filepath.Abs(input)
		if err != nil {
			return err
		}
		inputInfo, err := os.Stat(input)
		if err != nil {
			return err
		}
		if outPath == inputPath || (outInfo != nil && os.SameFile(outInfo, inputInfo)) {
			return fmt.Errorf("output aliases input %q", input)
		}
	}
	return nil
}
