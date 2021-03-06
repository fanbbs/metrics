// Copyright 2015 - 2016 Square Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package command

import (
	netcontext "context"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/square/metrics/api"
	"github.com/square/metrics/function"
	"github.com/square/metrics/function/registry"
	"github.com/square/metrics/inspect"
	"github.com/square/metrics/metric_metadata"
	"github.com/square/metrics/query/natural_sort"
	"github.com/square/metrics/query/predicate"
	"github.com/square/metrics/timeseries"
)

// ExecutionContext is the context supplied when invoking a command.
type ExecutionContext struct {
	TimeseriesStorageAPI  timeseries.StorageAPI // the backend
	MetricMetadataAPI     metadata.MetricAPI    // the api
	FetchLimit            int                   // the maximum number of fetches
	Timeout               time.Duration         // optional
	Registry              function.Registry     // optional
	SlotLimit             int                   // optional (0 => default 1000)
	Profiler              *inspect.Profiler     // optional
	AdditionalConstraints predicate.Predicate   // optional. Additional contrains for describe and select commands

	Ctx netcontext.Context
}

type Result struct {
	Body     interface{}
	Metadata map[string]interface{}
}

// Command is the final result of the parsing.
// A command contains all the information to execute the
// given query against the API.
type Command interface {
	// Execute the given command. Returns JSON-encodable result or an error.
	Execute(ExecutionContext) (Result, error)
	Name() string
}

// DescribeCommand describes the tag set managed by the given metric indexer.
type DescribeCommand struct {
	MetricName api.MetricKey
	Predicate  predicate.Predicate
}

// DescribeAllCommand returns all the metrics available in the system.
type DescribeAllCommand struct {
	Matcher *regexp.Regexp
}

// DescribeMetricsCommand returns all metrics that use a particular key-value pair.
type DescribeMetricsCommand struct {
	TagKey   string
	TagValue string
}

type SelectContext struct {
	Start        int64                   // Start of data timerange
	End          int64                   // End of data timerange
	Resolution   int64                   // Resolution of data timerange
	SampleMethod timeseries.SampleMethod // to use when up/downsampling to match requested resolution
}

// SelectCommand is the bread and butter of the metrics query engine.
// It actually performs the query against the underlying metrics system.
type SelectCommand struct {
	Predicate   predicate.Predicate
	Expressions []function.Expression
	Context     SelectContext
}

// Execute returns the list of tags satisfying the provided predicate.
func (cmd *DescribeCommand) Execute(context ExecutionContext) (Result, error) {

	// We generate a simple update function that closes around the profiler
	// so if we do have a cache miss it's correctly reported on this request.

	tagsets, err := context.MetricMetadataAPI.GetAllTags(cmd.MetricName, metadata.Context{
		Profiler: context.Profiler,
	})
	if err != nil {
		return Result{}, err
	}

	// Splitting each tag key into its own set of values is helpful for discovering actual metrics.
	predicate := predicate.All(cmd.Predicate, context.AdditionalConstraints)
	keyValueSets := map[string]map[string]bool{} // a map of tag_key => Set{tag_value}.
	for _, tagset := range tagsets {
		if predicate.Apply(tagset) {
			// Add each key as needed
			for key, value := range tagset {
				if keyValueSets[key] == nil {
					keyValueSets[key] = map[string]bool{}
				}
				keyValueSets[key][value] = true // add `value` to the set for `key`
			}
		}
	}
	keyValueLists := map[string][]string{} // a map of tag_key => list[tag_value]
	for key, set := range keyValueSets {
		list := make([]string, 0, len(set))
		for value := range set {
			list = append(list, value)
		}
		// sort the result
		natural_sort.Sort(list)
		keyValueLists[key] = list
	}
	return Result{Body: keyValueLists}, nil
}

func (cmd *DescribeCommand) Name() string {
	return "describe"
}

// Execute of a DescribeAllCommand returns the list of all metrics.
func (cmd *DescribeAllCommand) Execute(context ExecutionContext) (Result, error) {
	result, err := context.MetricMetadataAPI.GetAllMetrics(metadata.Context{
		Profiler: context.Profiler,
	})
	if err == nil {
		filtered := make([]api.MetricKey, 0, len(result))
		for _, row := range result {
			if cmd.Matcher.MatchString(string(row)) {
				filtered = append(filtered, row)
			}
		}
		sort.Sort(api.MetricKeys(filtered))
		return Result{
			Body: filtered,
			Metadata: map[string]interface{}{
				"count": len(filtered),
			},
		}, nil
	}
	return Result{}, err
}

func (cmd *DescribeAllCommand) Name() string {
	return "describe all"
}

// Execute asks for all metrics with the given name.
func (cmd *DescribeMetricsCommand) Execute(context ExecutionContext) (Result, error) {
	data, err := context.MetricMetadataAPI.GetMetricsForTag(cmd.TagKey, cmd.TagValue, metadata.Context{
		Profiler: context.Profiler,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{
		Body: data,
		Metadata: map[string]interface{}{
			"count": len(data),
		},
	}, nil
}

func (cmd *DescribeMetricsCommand) Name() string {
	return "describe metrics"
}

type QueryResult struct {
	Query string `json:"query"`
	Name  string `json:"name"`
	Type  string `json:"type"` // one of "series" or "scalars"
	// for "series" type
	Series    []api.Timeseries `json:"series"`
	Timerange api.Timerange    `json:"timerange,omitempty"`
	// for "scalar" type
	Scalars []function.TaggedScalar `json:"scalars,omitempty"`
}

// Execute performs the query represented by the given query string, and returs the result.
func (cmd *SelectCommand) Execute(context ExecutionContext) (Result, error) {
	userTimerange, err := api.NewSnappedTimerange(cmd.Context.Start, cmd.Context.End, cmd.Context.Resolution)
	if err != nil {
		return Result{}, err
	}
	slotLimit := context.SlotLimit
	defaultLimit := 1000
	if slotLimit == 0 {
		slotLimit = defaultLimit // the default limit
	}

	smallestResolution := userTimerange.Duration() / time.Duration(slotLimit-2)
	// ((end + res/2) - (start - res/2)) / res + 1 <= slots // make adjustments for a snap that moves the endpoints
	// (do some algebra)
	// (end - start + res) + res <= slots * res
	// end - start <= res * (slots - 2)
	// so
	// res >= (end - start) / (slots - 2)

	earliest := new(time.Time)
	*earliest = userTimerange.Start()

	widening := function.WidestMode{
		Registry:   context.Registry,
		Current:    userTimerange.Start(),
		Earliest:   earliest,
		Resolution: userTimerange.Resolution(),
		Mutex:      &sync.Mutex{},
	}
	if context.Registry == nil {
		widening.Registry = registry.Default()
	}
	for _, expression := range cmd.Expressions {
		_ = expression.ExpressionDescription(widening) // widen by each expression
	}

	widenedTimerange, err := api.NewSnappedTimerange(earliest.UnixNano()/1e6, userTimerange.EndMillis(), userTimerange.ResolutionMillis())

	if err != nil {
		// If the timerange is invalid, just fall back on the user's.
		// It's unlikely that this can actually occur; but just to be safe, it's an easy fallback.
		widenedTimerange = userTimerange
	}

	// Update the timerange by applying the insights of the storage API:
	chosenResolution, err := context.TimeseriesStorageAPI.ChooseResolution(widenedTimerange, smallestResolution)
	if err != nil {
		return Result{}, err
	}

	chosenTimerange, err := api.NewSnappedTimerange(userTimerange.StartMillis(), userTimerange.EndMillis(), int64(chosenResolution/time.Millisecond))
	if err != nil {
		return Result{}, err
	}

	if chosenTimerange.Slots() > slotLimit {
		return Result{}, function.NewLimitError(
			"Requested number of data points exceeds the configured limit",
			chosenTimerange.Slots(), slotLimit)
	}

	ctx, cancelFunc := context.Ctx, netcontext.CancelFunc(nil)

	if context.Timeout != 0 {
		ctx, cancelFunc = netcontext.WithTimeout(ctx, context.Timeout)
	}

	if cancelFunc != nil {
		// When this function returns, the context's resources will be cleaned up,
		// just in case something remains open.
		defer cancelFunc()
	}

	r := context.Registry
	if r == nil {
		r = registry.Default()
	}

	evaluationContext := function.EvaluationContextBuilder{
		MetricMetadataAPI:    context.MetricMetadataAPI,
		FetchLimit:           function.NewFetchCounter(context.FetchLimit),
		TimeseriesStorageAPI: context.TimeseriesStorageAPI,
		Predicate:            predicate.All(cmd.Predicate, context.AdditionalConstraints),
		SampleMethod:         cmd.Context.SampleMethod,
		Timerange:            chosenTimerange,

		Registry:        r,
		Profiler:        context.Profiler,
		EvaluationNotes: new(function.EvaluationNotes),

		Ctx: ctx,
	}.Build()

	results := make(chan []function.Value, 1)
	errors := make(chan error, 1)
	// Goroutines are never garbage collected, so we need to provide capacity so that the send always succeeds.
	go func() {
		// Evaluate the result, and send it along the goroutines.
		result, err := function.EvaluateMany(evaluationContext, cmd.Expressions)
		if err != nil {
			errors <- err
			return
		}
		results <- result
	}()
	select {
	case <-ctx.Done():
		return Result{}, function.NewLimitError("Timeout while executing the query.", context.Timeout, context.Timeout)
	case err := <-errors:
		return Result{}, err
	case result := <-results:
		description := map[string][]string{}
		for _, value := range result {
			listValue, err := value.ToSeriesList(evaluationContext.Timerange())
			if err != nil {
				continue
			}
			list := api.SeriesList(listValue)
			for _, series := range list.Series {
				for key, value := range series.TagSet {
					description[key] = append(description[key], value)
				}
			}
		}
		for key, values := range description {
			natural_sort.Sort(values)
			filtered := []string{}
			for i := range values {
				if i == 0 || values[i-1] != values[i] {
					filtered = append(filtered, values[i])
				}
			}
			description[key] = filtered
		}

		// Body adds the Query as an annotation.
		// It's a slice of interfaces; it will be cast to an interface
		// when returned from this function in a Result.
		body := make([]QueryResult, len(result))
		for i := range body {
			if list, ok := result[i].(function.SeriesListValue); ok {
				body[i] = QueryResult{
					Query:     cmd.Expressions[i].ExpressionDescription(function.StringQuery()),
					Name:      cmd.Expressions[i].ExpressionDescription(function.StringName()),
					Type:      "series",
					Series:    list.Series,
					Timerange: chosenTimerange,
				}
				continue
			}
			if scalars, err := result[i].ToScalarSet(); err == nil {
				body[i] = QueryResult{
					Query:   cmd.Expressions[i].ExpressionDescription(function.StringQuery()),
					Name:    cmd.Expressions[i].ExpressionDescription(function.StringName()),
					Type:    "scalars",
					Scalars: scalars,
				}
				continue
			}
			return Result{}, fmt.Errorf("query %s does not result in a timeseries or scalar.", cmd.Expressions[i].ExpressionDescription(function.StringQuery))
		}

		return Result{
			Body: body,
			Metadata: map[string]interface{}{
				"description": description,
				"notes":       evaluationContext.Notes(),
				"resolution":  chosenResolution,
			},
		}, nil
	}
}

func (cmd *SelectCommand) Name() string {
	return "select"
}

//ProfilingCommand is a Command that also performs profiling actions.
type ProfilingCommand struct {
	Profiler *inspect.Profiler
	Command  Command
}

func NewProfilingCommandWithProfiler(command Command, profiler *inspect.Profiler) Command {
	return ProfilingCommand{
		Profiler: profiler,
		Command:  command,
	}
}

func (cmd ProfilingCommand) Name() string {
	return cmd.Command.Name()
}

func (cmd ProfilingCommand) Execute(context ExecutionContext) (Result, error) {
	defer cmd.Profiler.Record(fmt.Sprintf("%s.Execute", cmd.Name()))()
	context.Profiler = cmd.Profiler
	result, err := cmd.Command.Execute(context)
	if err != nil {
		return Result{}, err
	}
	profiles := cmd.Profiler.All()
	if len(profiles) != 0 {
		if result.Metadata == nil {
			result.Metadata = map[string]interface{}{}
		}
		result.Metadata["profile"] = profiles
	}
	return result, nil
}
