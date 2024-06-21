//
// Copyright 2020 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/newrelic/newrelic-php-agent/daemon/internal/newrelic/log"
	"github.com/newrelic/newrelic-php-agent/daemon/internal/newrelic/sysinfo"

	"github.com/newrelic/newrelic-php-agent/daemon/internal/newrelic"
)

// A Test captures the input, output and outcome of an integration test.
type Test struct {
	Name string
	Path string
	Desc string

	// Expected Data
	analyticEvents   []byte
	customEvents     []byte
	errorEvents      []byte
	spanEvents       []byte
	spanEventsLike   []byte
	logEvents        []byte
	metrics          []byte
	metricsExist     []byte
	metricsDontExist []byte
	slowSQLs         []byte
	tracedErrors     []byte
	txnTraces        []byte

	// Expected Output
	expect                []byte
	expectRegex           []byte
	expectScrubbed        []byte
	expectResponseHeaders http.Header

	// do we expect this test to produce a harvest
	expectHarvest bool

	// PHP packages tests have a config controlling how
	// the actual packages installed is detected
	phpPackagesConfig []byte

	// Raw parsed test information used to construct the Tx.
	// The settings and env do not include global env and
	// global settings.
	rawSkipIf  []byte
	Env        map[string]string
	Settings   map[string]string
	PhpModules map[string]string
	headers    http.Header

	// When non-empty describes why failed should be true after the test
	// is run. This field may be set in the test definition to indicate
	// it should always fail, or it may be set at runtime if the first
	// line of the test's output starts with "xfail:".
	Xfail string

	// Remaining fields are populated after the test is run.
	Skipped bool
	Warned  bool

	// If the test was skipped or the test could not be run due to an
	// error, describes the reason.
	Err error

	Output []byte

	// Response headers send through the integration runner HTTP endpoint
	// during the test run.
	ResponseHeaders http.Header

	// The timed duration of the test. If no timing was done, this is set
	// to 0.
	Duration time.Duration

	// Adaptions made to the configuration of C Agent tests.
	Config string

	// If the test ran to completion, contains one element for each
	// failed expectation.
	Failed   bool
	Failures []error

	// Notes are comments generated by tests which are displayed in results
	Notes []string
}

func NewTest(name string) *Test {
	test := &Test{Name: name, Env: make(map[string]string)}
	test.SetExpectHarvest(true)
	return test
}

type ComparisonFailure struct {
	Name   string
	Expect string
	Actual string
}

func (c ComparisonFailure) Error() string {
	return fmt.Sprintf("%s error:\nexpected:\n%s\n\nactual:\n%s\n", c.Name, c.Expect, c.Actual)
}

func (t *Test) SetExpectHarvest(value bool) {
	t.expectHarvest = value
}

func (t *Test) GetExpectHarvest() bool {
	return t.expectHarvest
}

func (t *Test) IsWeb() bool {
	_, found := t.Env["REQUEST_METHOD"]
	return found || len(t.headers) > 0
}

func (t *Test) IsPHP() bool {
	return strings.HasSuffix(t.Path, ".php")
}

func (t *Test) ShouldCheckResponseHeaders() bool {
	return nil != t.expectResponseHeaders
}

// Skip marks the test as skipped and records the given reason.
func (t *Test) Skip(reason string) {
	t.Skipped = true
	t.Err = errors.New(reason)
}

// Skipf marks the test as skipped and formats its arguments
// according to the format, and records the text as the reason.
func (t *Test) Skipf(format string, args ...interface{}) {
	t.Skipped = true
	t.Err = fmt.Errorf(format, args...)
}

// Warn marks the test as unable to be run and records the given reason.
func (t *Test) Warn(reason string) {
	t.Warned = true
	t.Err = errors.New(reason)
}

// Warnf marks the test as unable to be run  and formats its arguments
// according to the format, and records the text as the reason.
func (t *Test) Warnf(format string, args ...interface{}) {
	t.Warned = true
	t.Err = fmt.Errorf(format, args...)
}

// Fail records an unsatisified expectation for the test and marks
// the test as failed.
func (t *Test) Fail(err error) {
	t.Failed = true
	t.Failures = append(t.Failures, err)
}

// Fatal records an error which prevented the test from being
// completed and marks the test as failed.
func (t *Test) Fatal(err error) {
	t.Failed = true
	t.Err = err
}

// XFail marks the test as expected to fail and records the given reason.
func (t *Test) XFail(reason string) {
	t.Xfail = reason
}

// AddNote adds a note to the output for the final test status
func (t *Test) AddNote(note string) {
	t.Notes = append(t.Notes, note)
}

func merge(a, b map[string]string) map[string]string {
	merged := make(map[string]string)
	for k, v := range a {
		merged[k] = v
	}
	for k, v := range b {
		merged[k] = v
	}
	return merged
}

func (t *Test) MakeRun(ctx *Context) (Tx, error) {
	t.Env = merge(ctx.Env, t.Env)
	settings := merge(ctx.Settings, t.Settings)
	settings["newrelic.appname"] = t.Name

	headers := make(http.Header)
	for key, vals := range t.headers {
		for _, v := range vals {
			expanded := t.subEnvVars([]byte(v))
			headers.Set(key, string(expanded))
		}
	}

	settings = merge(settings, t.PhpModules)

	if t.IsC() {
		return CTx(ScriptFile(t.Path), t.Env, settings, headers, ctx)
	}
	if t.IsWeb() {
		return CgiTx(ScriptFile(t.Path), t.Env, settings, headers, ctx)
	}
	return PhpTx(ScriptFile(t.Path), t.Env, settings, ctx)
}

func (t *Test) MakeSkipIf(ctx *Context) (Tx, error) {
	if 0 == len(t.rawSkipIf) {
		return nil, nil
	}

	t.Env = merge(ctx.Env, t.Env)
	settings := merge(ctx.Settings, t.Settings)
	settings["newrelic.appname"] = "skipif"

	// foo/bar/test.php -> foo/bar/test.skipif.php
	ext := filepath.Ext(t.Path)
	src := &ScriptFragment{
		name: strings.TrimSuffix(t.Path, ext) + ".skipif",
		data: t.rawSkipIf,
	}

	return PhpTx(src, t.Env, settings, ctx)
}

type Scrub struct {
	re          *regexp.Regexp
	replacement []byte
}

var scrubs = []Scrub{
	{re: regexp.MustCompile(`line\s+\d+`), replacement: []byte("line ??")},
	{re: regexp.MustCompile(`php\s+\(\d+\)`), replacement: []byte("php (??)")},
	{re: regexp.MustCompile(`php\(\d+\)`), replacement: []byte("php(??)")},
	{re: regexp.MustCompile(`\.inc\s\(\d+\)`), replacement: []byte(".inc (??)")},
	{re: regexp.MustCompile(`php:\d+`), replacement: []byte("php:??")},
}

func ScrubLineNumbers(in []byte) []byte {
	for _, r := range scrubs {
		in = r.re.ReplaceAll(in, r.replacement)
	}

	return in
}

func ScrubFilename(in []byte, filename string) []byte {
	filenameEscapedSlashes := strings.Replace(filename, `/`, `\/`, -1)
	out := bytes.Replace(in, []byte(filename), []byte("__FILE__"), -1)
	out = bytes.Replace(out, []byte(filenameEscapedSlashes), []byte("__FILE__"), -1)
	out = bytes.Replace(out, []byte(`\/`+filepath.Base(filename)), []byte("__FILE__"), -1)
	out = bytes.Replace(out, []byte(`/`+filepath.Base(filename)), []byte("__FILE__"), -1)
	out = bytes.Replace(out, []byte(filepath.Base(filename)), []byte("__FILE__"), -1)

	return out
}

func ScrubHost(in []byte) []byte {
	host, err := sysinfo.Hostname()
	if err != nil {
		log.Debugf("unable to determine hostname: %v", err)
		return in
	}

	re := regexp.MustCompile("\\b" + regexp.QuoteMeta(host) + "\\b")
	return re.ReplaceAll(in, []byte("__HOST__"))
}

func (t *Test) subEnvVars(in []byte) []byte {
	re := regexp.MustCompile("ENV\\[.*?\\]")
	return re.ReplaceAllFunc(in, func(match []byte) []byte {
		k := string(match[4 : len(match)-1])
		v, present := t.Env[k]
		if !present {
			v = os.Getenv(k)
		}
		return []byte(v)
	})
}

// Response headers have to be compared in this verbose way to support the "??"
// wildcard.
func (t *Test) compareResponseHeaders() {
	if false == t.ShouldCheckResponseHeaders() {
		return
	}

	failure := func() {
		expected, _ := json.Marshal(t.expectResponseHeaders)
		actual, _ := json.Marshal(t.ResponseHeaders)
		t.Fatal(ComparisonFailure{Name: "response headers", Expect: string(expected), Actual: string(actual)})
	}

	if len(t.expectResponseHeaders) != len(t.ResponseHeaders) {
		failure()
		return
	}

	for key, values := range t.ResponseHeaders {
		expectValues := t.expectResponseHeaders[key]

		if nil == expectValues {
			failure()
			return
		}

		if len(expectValues) != len(values) {
			failure()
			return
		}

		for i := 0; i < len(values); i++ {
			if expectValues[i] == "??" {
				continue
			}
			if expectValues[i] != values[i] {
				failure()
				return
			}
		}
	}
}

// Handling EXPECT_SPAN_EVENTS_LIKE is different than normal payload compare so
// a different function is required
func (t *Test) compareSpanEventsLike(harvest *newrelic.Harvest) {
	// convert array of expected spans JSON to interface representation
	var x2 interface{}

	if nil == t.spanEventsLike {
		return
	}

	if err := json.Unmarshal(t.subEnvVars(t.spanEventsLike), &x2); nil != err {
		t.Fatal(fmt.Errorf("unable to parse expected spans like json for fuzzy matching: %v", err))
		return
	}

	// expected will be represented as an array of "interface{}"
	// each element will be the internal representation of the JSON
	// for each expected span
	// this is needed for the call to isFuzzyMatch Recursive later
	// when comparing to the actual spans in their internal representation
	expected := x2.([]interface{})

	// now parse actual span data JSON into interface representation
	var x1 interface{}

	es := *harvest.SpanEvents
	id := newrelic.AgentRunID("?? agent run id")
	actualJSON, err := es.Data(id, time.Now())
	if nil != err {
		t.Fatal(fmt.Errorf("unable to access span event JSON data: %v", err))
		return
	}

	// scrub actual spans
	scrubjson := ScrubLineNumbers(actualJSON)
	scrubjson = ScrubFilename(scrubjson, t.Path)
	scrubjson = ScrubHost(scrubjson)

	// parse to internal format
	if err := json.Unmarshal(scrubjson, &x1); nil != err {
		t.Fatal(fmt.Errorf("unable to parse actual spans like json for fuzzy matching: %v", err))
		return
	}

	// expect x1 to be of type "[]interface {}" which wraps the entire span event data
	// within this generic array there will be:
	// - a string of the form "?? agent run id"
	// - a map container "events_seen" and "reservoir_size"
	// - an array of arrays containing:
	//    - map containing main span data
	//    - map for attributes(?)
	//    - map of CLM data

	// test initial type is as expected
	switch x1.(type) {
	case []interface{}:
	default:
		t.Fatal(errors.New("span event data json doesnt match expected format"))
		return
	}

	// expect array of len 3
	v2, _ := x1.([]interface{})
	if 3 != len(v2) {
		t.Fatal(errors.New("span event data json doesnt match expected format - expected 3 elements"))
		return
	}

	// get array of actual spans from 3rd element
	actual := v2[2].([]interface{})

	// check if expected JSON is present in actual data
	// will call isFuzzyMatchRecursive with "interface" representations
	numMatched := 0
	haveMatched := make([]bool, len(expected))
	for i := 0; i < len(expected); i++ {
		haveMatched[i] = false
	}

	for i := 0; i < len(actual); i++ {
		// check each expected span (interface representation) against current actual span
		// only iterate over unmatched expected spans
		for j := 0; j < len(expected); j++ {
			if haveMatched[j] {
				continue
			}

			err := isFuzzyMatchRecursive(expected[j], actual[i])
			if nil == err {
				haveMatched[j] = true
				numMatched++
				break
			}
		}

		if len(expected) == numMatched {
			break
		}
	}

	for j := 0; j < len(expected); j++ {
		if !haveMatched[j] {
			actualPretty := bytes.Buffer{}
			json.Indent(&actualPretty, actualJSON, "", "  ")
			expectedJSON, _ := json.Marshal(expected[j])

			t.Fail(ComparisonFailure{
				Name:   fmt.Sprintf("matching span event data like: unmatched expected span"),
				Expect: string(expectedJSON),
				Actual: actualPretty.String(),
			})
			return
		}
	}
}

func (t *Test) comparePayload(expected json.RawMessage, pc newrelic.PayloadCreator, isMetrics bool) {
	if nil == expected {
		// No expected output has been specified:  Anything passes.
		return
	}

	id := newrelic.AgentRunID("?? agent run id")
	cmd := pc.Cmd()

	audit, err := newrelic.IntegrationData(pc, id, time.Now())
	if nil != err {
		t.Fatal(err)
		return
	}

	if "null" == strings.TrimSpace(string(expected)) {
		// The absence of output is expected.
		if !pc.Empty() {
			t.Fatal(fmt.Errorf("error matching %v: expected null, got %v",
				cmd, string(audit)))
		}
		return
	}

	if pc.Empty() {
		t.Fail(ComparisonFailure{
			Name:   "matching " + cmd,
			Expect: string(expected),
			Actual: "<nil>",
		})
		return
	}

	actual := ScrubLineNumbers(audit)
	actual = ScrubFilename(actual, t.Path)
	actual = ScrubHost(actual)

	if isMetrics {
		var err error
		actual, err = newrelic.OrderScrubMetrics(actual, MetricScrubRegexps)
		if nil != err {
			t.Fatal(fmt.Errorf("unable to order actual metrics: %s", err))
			return
		}
	}

	expected = t.subEnvVars(expected)

	err = IsFuzzyMatch(expected, actual)
	if nil != err {
		actualPretty := bytes.Buffer{}
		json.Indent(&actualPretty, actual, "", "  ")

		t.Fail(ComparisonFailure{
			Name:   fmt.Sprintf("matching %v: %v", cmd, err),
			Expect: string(expected),
			Actual: actualPretty.String(),
		})
	}
}

func (t *Test) comparePhpPackages(harvest *newrelic.Harvest) {
	var expectedPackages []PhpPackage
	var expectedPkgsCollection *PhpPackagesCollection
	var expectNullPkgs bool
	var version_overrides map[string]interface{}

	if nil != t.phpPackagesConfig {
		var err error

		expectNullPkgs = "null" == string(t.phpPackagesConfig)
		if expectNullPkgs {
			expectedPackages = nil
		} else {
			expectedPkgsCollection, err = NewPhpPackagesCollection(t.Path, t.phpPackagesConfig)
			if nil != err {
				t.Fatal(err)
				return
			}

			expectedPackages, err = expectedPkgsCollection.GatherInstalledPackages()
			if nil != err {
				t.Fatal(err)
				return
			}
		}
	} else {
		// no configuration given for package (no EXPECT_PHP_PACKAGES in test case) so don't run test
		return
	}

	if !expectNullPkgs && (nil == expectedPackages || 0 == len(expectedPackages)) {
		t.Fatal(fmt.Errorf("EXPECTED_PHP_PACKAGES used but no packages detected in environment!"))
	}

	// load any expected versions overrides
	if 0 < len(expectedPkgsCollection.OverrideVersionsFile()) {
		var err error
		version_overrides, err = ParseOverrideVersionsFile(t.Path, expectedPkgsCollection.OverrideVersionsFile())
		if nil != err {
			t.Fatal(err)
		}
	}

	audit, err := newrelic.IntegrationData(harvest.PhpPackages, newrelic.AgentRunID("?? agent run id"), time.Now())
	if nil != err {
		t.Fatal(err)
		return
	}
	actualPackages, err := GetPhpPackagesFromData(audit)
	if nil != err {
		t.Fatal(err)
		return
	}

	if nil != harvest.PhpPackages && 0 < harvest.PhpPackages.NumSaved() {
		if nil == expectedPackages {
			t.Fail(fmt.Errorf("No expected PHP packages, harvest contains %+v\n", actualPackages))
		}
		// compare expected and actual lists of packages
		// since package names should be identical, iterate over
		// expected list and compare element by element with same
		// position in actual list.  Name and version should match.
		// this works because the functions which generate these
		// lists sort them by package name for us
		if len(expectedPackages) != len(actualPackages) {
			t.Fail(fmt.Errorf("Expected and actual php packages differ in length %d vs %d: expected %+v actual %+v",
				len(expectedPackages), len(actualPackages), expectedPackages, actualPackages))
			return
		}
		for i, _ := range expectedPackages {
			if expectedPackages[i].Name == actualPackages[i].Name {
				testPackageNameOnly := false
				if nil != expectedPkgsCollection.config.packageNameOnly {
					testPackageNameOnly = StringSliceContains(expectedPkgsCollection.config.packageNameOnly, actualPackages[i].Name)
					if testPackageNameOnly {
						t.AddNote(fmt.Sprintf("Tested package name only for packages: %+v", expectedPkgsCollection.config.packageNameOnly))
					}
				}

				// see if a version override exists for the expectation
				expected_version := expectedPackages[i].Version
				override_version, ok := version_overrides[expected_version]
				if ok {
					t.AddNote(fmt.Sprintf("Overrode detected version %s with %s", expected_version, override_version))
					expected_version = override_version.(string)
				}

				if testPackageNameOnly {
					if " " != actualPackages[i].Version {
						t.Fail(fmt.Errorf("Expected no package version and a package version was detected - expected \" \" actual %+v. ",
							actualPackages[i].Version))
						return
					} else {
						continue
					}
				}

				if expected_version == actualPackages[i].Version {
					continue
				}
			}
			t.Fail(fmt.Errorf("Expected and actual Php packages do not match: expected %+v actual %+v. Complete expected %v actual %v.",
				expectedPackages[i], actualPackages[i], expectedPackages, actualPackages))
			return
		}
	} else {
		if nil != expectedPackages {
			t.Fail(fmt.Errorf("Expected PHP packages %+v, harvest contains none\n", expectedPackages))
		}
	}
}

var (
	MetricScrubRegexps = []*regexp.Regexp{
		regexp.MustCompile(`CPU/User Time`),
		regexp.MustCompile(`CPU/User/Utilization`),
		regexp.MustCompile(`Memory/Physical`),
		regexp.MustCompile(`Supportability/execute/user/call_count`),
		regexp.MustCompile(`Supportability/execute/allocated_segment_count`),
		regexp.MustCompile(`Memory/RSS`),
		regexp.MustCompile(`^Supportability\/Locale`),
		regexp.MustCompile(`^Supportability\/InstrumentedFunction`),
		regexp.MustCompile(`^Supportability\/TxnData\/.*`),
		regexp.MustCompile(`^Supportability/C/NewrelicVersion/.*`),
	}
)

func (t *Test) Compare(harvest *newrelic.Harvest) {
	if nil != t.expect {
		expect := string(bytes.TrimSpace(t.expect))
		output := string(bytes.TrimSpace(t.Output))

		if expect != output {
			t.Fail(ComparisonFailure{Name: "expect", Expect: expect, Actual: output})
		}
	}

	if nil != t.expectRegex {
		re, err := regexp.Compile(string(t.expectRegex))
		if nil != err {
			t.Fatal(fmt.Errorf("unable to compile expect regex %v: %v", string(t.expectRegex), err))
		} else {
			if !re.Match(t.Output) {
				t.Fail(ComparisonFailure{Name: "regex", Expect: string(t.expectRegex), Actual: string(t.Output)})
			}
		}
	}

	if nil != t.expectScrubbed {
		actual := string(bytes.TrimSpace(ScrubFilename(ScrubLineNumbers(t.Output), t.Path)))
		expect := string(bytes.TrimSpace(t.expectScrubbed))

		if expect != actual {
			t.Fail(ComparisonFailure{Name: "scrubbed", Expect: expect, Actual: actual})
		}
	}

	// if we expect no harvest (ex. an ignored transaction)
	// and there is not harvest, then we pass
	if nil == harvest && !t.GetExpectHarvest() {
		return
	}

	// if we expect no harvest (ex. an ignored transaction)
	// and there IS a harvest, then we've failed
	if nil != harvest && !t.GetExpectHarvest() {
		t.Fatal(errors.New("received a harvest, but EXPECT_HARVEST set to no"))
		return
	}

	// if we expect a harvest and there isn't one, then we've failed
	if nil == harvest && t.GetExpectHarvest() {
		t.Fatal(errors.New("no harvest received"))
		return
	}

	if nil != t.metricsExist {
		for _, name := range strings.Split(strings.TrimSpace(string(t.subEnvVars(t.metricsExist))), "\n") {
			name = strings.TrimSpace(name)
			expected := strings.Replace(name, "__FILE__", t.Path, -1)
			if !harvest.Metrics.Has(expected) {
				actualPretty := bytes.Buffer{}
				json.Indent(&actualPretty, []byte(harvest.Metrics.DebugJSON()), "", "  ")
				t.Fail(fmt.Errorf("metric does not exist: %s\n\nactual metric table: %s", expected, actualPretty.String()))
			}
		}
	}

	if nil != t.metricsDontExist {
		for _, name := range strings.Split(strings.TrimSpace(string(t.subEnvVars(t.metricsDontExist))), "\n") {
			name = strings.TrimSpace(name)
			expected := strings.Replace(name, "__FILE__", t.Path, -1)
			if harvest.Metrics.Has(expected) {
				actualPretty := bytes.Buffer{}
				json.Indent(&actualPretty, []byte(harvest.Metrics.DebugJSON()), "", "  ")
				t.Fail(fmt.Errorf("unexpected metric in harvest: %s\n\nactual metric table: %s", expected, actualPretty.String()))
			}
		}
	}

	// if we expect a harvest and these is not, then we run our tests as per normal
	t.compareResponseHeaders()

	// Ensure that the actual and expected metrics are in the same order.
	// Also scrub insignificant metrics, such as CPU, Supportability, etc.
	expectedMetrics, err := newrelic.OrderScrubMetrics(t.metrics, MetricScrubRegexps)
	if nil != err {
		t.Fatal(fmt.Errorf("unable to order expected metrics: %v", err))
		return
	}

	// check for any "expected spans like"
	t.compareSpanEventsLike(harvest)

	// check remaining payloads
	t.comparePayload(t.analyticEvents, harvest.TxnEvents, false)
	t.comparePayload(t.customEvents, harvest.CustomEvents, false)
	t.comparePayload(t.errorEvents, harvest.ErrorEvents, false)
	t.comparePayload(t.spanEvents, harvest.SpanEvents, false)
	t.comparePayload(t.logEvents, harvest.LogEvents, false)
	t.comparePayload(expectedMetrics, harvest.Metrics, true)
	t.comparePayload(t.slowSQLs, harvest.SlowSQLs, false)
	t.comparePayload(t.tracedErrors, harvest.Errors, false)
	t.comparePayload(t.txnTraces, harvest.TxnTraces, false)

	// check php packages
	t.comparePhpPackages(harvest)

	if t.Failed && t.expect == nil && t.expectRegex == nil && t.expectScrubbed == nil {
		// The test failed and there's no pre-existing expectation on the output
		output := string(bytes.TrimSpace(t.Output))
		if len(output) > 0 {
			t.Fail(fmt.Errorf("Test output (this might be important):\n%s\n", output))
		}
	}
}

// Reset discards the results of previous test executions.
func (t *Test) Reset() {
	t.Xfail = ""
	t.Skipped = false
	t.Warned = false
	t.Err = nil
	t.Output = nil
	t.Failed = false
	t.Failures = nil
	t.Notes = nil
}
