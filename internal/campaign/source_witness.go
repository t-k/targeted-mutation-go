package campaign

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

// SourceWitness is a test verified against the original source and a source edit.
type SourceWitness struct {
	Call     string `json:"call"`
	Original string `json:"original"`
	Mutated  string `json:"mutated"`
	Code     string `json:"code"`
	Verified bool   `json:"verified"`
}

const sourceWitnessEnvironment = "TMT_GO_SOURCE_WITNESS_OUTPUT"

type sourceWitnessTrial struct {
	test, file, marker string
	pkg                target.Package
	idents             shadowedIdents
}

func (s *session) sourceWitness(ctx context.Context, job sourceJob) (*SourceWitness, error) {
	calls := sourceWitnessCalls(job)
	if len(calls) == 0 {
		return nil, ctx.Err()
	}
	var trial sourceWitnessTrial
	for _, pkg := range s.packages {
		if slices.Contains(pkg.Paths, job.edit.File) {
			trial.pkg = pkg
			break
		}
	}
	if trial.pkg.Path == "" {
		return nil, nil
	}
	trial.idents = packageShadowedIdents(trial.pkg.Dir, job.source)
	dir, err := os.MkdirTemp(s.scratch, "source-witness-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	unique := hex.EncodeToString(nonce[:])
	trial.test = "TestTmtSourceWitness" + unique
	trial.file = filepath.Join(trial.pkg.Dir, "tmt_source_witness_"+unique+"_test.go")
	trial.marker = "source-witness-assertion-" + unique
	if _, err := os.Lstat(trial.file); !os.IsNotExist(err) {
		return nil, fmt.Errorf("synthetic source witness path is occupied: %s", trial.file)
	}
	original := target.Replacement{Path: job.edit.File, Data: job.source}
	mutated := job.materialize()
	collector := trial.collector(calls)
	before, err := s.collectSourceWitness(ctx, dir, trial, original, collector, len(calls))
	if err != nil || before == nil {
		return nil, err
	}
	after, err := s.collectSourceWitness(ctx, dir, trial, mutated, collector, len(calls))
	if err != nil || after == nil {
		return nil, err
	}
	for i, call := range calls {
		if before[i] == after[i] || !sourceWitnessLiteral(before[i]) || !sourceWitnessLiteral(after[i]) {
			continue
		}
		witness := SourceWitness{Call: call, Original: before[i], Mutated: after[i]}
		return s.verifySourceWitness(ctx, dir, trial, original, mutated, witness)
	}
	return nil, ctx.Err()
}

func (s *session) collectSourceWitness(ctx context.Context, dir string, trial sourceWitnessTrial, src target.Replacement, collector string, count int) ([]string, error) {
	run, data, err := s.runSourceWitness(ctx, dir, trial, src, collector)
	if err != nil || !sourceWitnessEvent(run, trial.test, "passed", "pass") {
		return nil, err
	}
	var values []string
	if json.Unmarshal(data, &values) != nil || len(values) != count {
		return nil, nil
	}
	return values, nil
}

func (s *session) verifySourceWitness(ctx context.Context, dir string, trial sourceWitnessTrial, original, mutated target.Replacement, witness SourceWitness) (*SourceWitness, error) {
	assertion := trial.assertion(witness, true)
	base, data, err := s.runSourceWitness(ctx, dir, trial, original, assertion)
	if err != nil || len(data) != 0 || !sourceWitnessEvent(base, trial.test, "passed", "pass") {
		return nil, err
	}
	changed, data, err := s.runSourceWitness(ctx, dir, trial, mutated, assertion)
	if err != nil || !bytes.Equal(data, []byte(trial.marker+"\n"+witness.Mutated)) ||
		!sourceWitnessEvent(changed, trial.test, "failed", "fail") {
		return nil, err
	}
	code, err := format.Source([]byte(trial.assertion(witness, false)))
	if err != nil {
		return nil, err
	}
	witness.Code = string(code)
	witness.Verified = true
	return &witness, nil
}

func sourceWitnessEvent(run commandResult, test, kind, event string) bool {
	return run.kind == kind && slices.Equal(run.tests, []string{test}) &&
		slices.Equal(run.events, []string{"run:" + test, event + ":" + test})
}

func (s *session) runSourceWitness(ctx context.Context, dir string, trial sourceWitnessTrial, src target.Replacement, generated string) (commandResult, []byte, error) {
	if err := ctx.Err(); err != nil {
		return commandResult{}, nil, err
	}
	runDir, err := os.MkdirTemp(dir, "trial-")
	if err != nil {
		return commandResult{}, nil, err
	}
	defer os.RemoveAll(runDir)
	overlay, err := makeOverlay(runDir, []target.Replacement{src, {Path: trial.file, Data: []byte(generated), Synthetic: true}})
	if err != nil {
		return commandResult{}, nil, err
	}
	execDir, err := os.MkdirTemp(runDir, "cwd-")
	if err != nil {
		return commandResult{}, nil, err
	}
	bin := consumer{pkg: trial.pkg.Path, dir: execDir, binary: filepath.Join(runDir, "witness.test")}
	if err := s.build(ctx, overlay, &bin, 1); err != nil {
		// A source candidate or the additional test may be unbuildable.
		return commandResult{}, nil, ctx.Err()
	}
	output := filepath.Join(runDir, "observed")
	env := cleanEnv("PATH="+filepath.Join(s.env.GOROOT, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		sourceWitnessEnvironment+"="+output)
	command := runCommand(ctx, s.cfg.Timeout, execDir, env, bin.binary,
		"-test.v=test2json", "-test.timeout="+s.cfg.Timeout.String(), "-test.run=^"+trial.test+"$")
	if err := ctx.Err(); err != nil {
		return command, nil, err
	}
	data, err := readSourceWitnessOutput(output)
	return command, data, err
}

func (trial sourceWitnessTrial) expectedExpr(original string) string {
	if trial.idents.hasBool {
		switch original {
		case "true":
			return "(0 == 0)"
		case "false":
			return "(0 != 0)"
		}
	}
	return original
}

func readSourceWitnessOutput(path string) ([]byte, error) {
	const limit = 128 * 1024
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if len(data) > limit {
		return nil, nil
	}
	return data, err
}

func (trial sourceWitnessTrial) collector(calls []string) string {
	var body strings.Builder
	param := "tmt" + trial.test + "T"
	fmt.Fprintf(&body, "package %s\n", trial.pkg.Name)
	body.WriteString(`import (
	tmtWitnessJSON "encoding/json"
	tmtWitnessMath "math"
	tmtWitnessOS "os"
	tmtWitnessReflect "reflect"
	tmtWitnessStrconv "strconv"
	tmtWitnessTesting "testing"
)
`)
	fmt.Fprintf(&body, "func %s(%s *tmtWitnessTesting.T) {\n", trial.test, param)
	fmt.Fprintf(&body, `tmtCapture := func(tmtCall func() interface{}) (tmtResult string) {
	defer func() { _ = recover() }()
	return %s(tmtCall())
}
`, trial.literalName())
	body.WriteString("tmtValues := []string{\n")
	for _, call := range calls {
		fmt.Fprintf(&body, "tmtCapture(func() interface{} { return %s }),\n", call)
	}
	fmt.Fprintf(&body, `}
tmtData, tmtErr := tmtWitnessJSON.Marshal(tmtValues)
if tmtErr != nil { %s.Fatal(tmtErr) }
if tmtErr := tmtWitnessOS.WriteFile(tmtWitnessOS.Getenv(%q), tmtData, 0600); tmtErr != nil { %s.Fatal(tmtErr) }
}
`, param, sourceWitnessEnvironment, param)
	body.WriteString(trial.literalFunction())
	return body.String()
}

func (trial sourceWitnessTrial) assertion(witness SourceWitness, proof bool) string {
	var body strings.Builder
	fmt.Fprintf(&body, "package %s\n", trial.pkg.Name)
	expected := trial.expectedExpr(witness.Original)
	if proof {
		param := "tmt" + trial.test + "T"
		gotVar := "tmt" + trial.test + "Got"
		body.WriteString(`import (
	tmtWitnessMath "math"
	tmtWitnessOS "os"
	tmtWitnessReflect "reflect"
	tmtWitnessStrconv "strconv"
	tmtWitnessTesting "testing"
)
`)
		fmt.Fprintf(&body, "func %s(%s *tmtWitnessTesting.T) {\nif %s := %s; %s != %s {\n",
			trial.test, param, gotVar, witness.Call, gotVar, expected)
		fmt.Fprintf(&body, "if tmtErr := tmtWitnessOS.WriteFile(tmtWitnessOS.Getenv(%q), []byte(%q+%s(%s)), 0600); tmtErr != nil { %s.Fatal(tmtErr) }\n",
			sourceWitnessEnvironment, trial.marker+"\n", trial.literalName(), gotVar, param)
		fmt.Fprintf(&body, "%s.Fatalf(%q, %s)\n}\n}\n", param, "got %v, want "+strings.ReplaceAll(witness.Original, "%", "%%"), gotVar)
		body.WriteString(trial.literalFunction())
		return body.String()
	}
	testingType, param, gotVar := "testing.T", "t", "got"
	if trial.idents.hasTesting {
		body.WriteString("import tmtWitnessTesting \"testing\"\n")
		testingType = "tmtWitnessTesting.T"
	} else {
		body.WriteString("import \"testing\"\n")
	}
	if trial.idents.hasT || witnessUsesIdent(witness.Call, "t") {
		param = "tmtT"
		if witnessUsesIdent(witness.Call, param) {
			param = "tmt" + trial.test + "T"
		}
	}
	if trial.idents.hasGot || witnessUsesIdent(witness.Call, "got") {
		gotVar = "tmtGot"
		if witnessUsesIdent(witness.Call, gotVar) {
			gotVar = "tmt" + trial.test + "Got"
		}
	}
	fmt.Fprintf(&body, "func %s(%s *%s) {\nif %s := %s; %s != %s {\n", trial.test, param, testingType, gotVar, witness.Call, gotVar, expected)
	fmt.Fprintf(&body, "%s.Fatalf(%q, %s)\n}\n}\n", param, "got %v, want "+strings.ReplaceAll(witness.Original, "%", "%%"), gotVar)
	return body.String()
}

func (trial sourceWitnessTrial) literalName() string {
	return "tmt" + trial.test + "Literal"
}

func (trial sourceWitnessTrial) literalFunction() string {
	return "func " + trial.literalName() + `(value interface{}) string {
	v := tmtWitnessReflect.ValueOf(value)
	switch v.Kind() {
	case tmtWitnessReflect.Bool:
		return tmtWitnessStrconv.FormatBool(v.Bool())
	case tmtWitnessReflect.String:
		if len(v.String()) <= 128 { return tmtWitnessStrconv.Quote(v.String()) }
	case tmtWitnessReflect.Int, tmtWitnessReflect.Int8, tmtWitnessReflect.Int16, tmtWitnessReflect.Int32, tmtWitnessReflect.Int64:
		return tmtWitnessStrconv.FormatInt(v.Int(), 10)
	case tmtWitnessReflect.Uint, tmtWitnessReflect.Uint8, tmtWitnessReflect.Uint16, tmtWitnessReflect.Uint32, tmtWitnessReflect.Uint64, tmtWitnessReflect.Uintptr:
		return tmtWitnessStrconv.FormatUint(v.Uint(), 10)
	case tmtWitnessReflect.Float32, tmtWitnessReflect.Float64:
		f := v.Float()
		if f == 0 { return "0" }
		if !tmtWitnessMath.IsNaN(f) && !tmtWitnessMath.IsInf(f, 0) {
			return tmtWitnessStrconv.FormatFloat(f, 'g', -1, v.Type().Bits())
		}
	}
	return ""
}
`
}
