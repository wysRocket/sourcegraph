package jobutil

import (
	"context"
	"github.com/sourcegraph/sourcegraph/internal/gitserver"
	"github.com/sourcegraph/sourcegraph/internal/search/result"

	"go.opentelemetry.io/otel/attribute"

	"github.com/sourcegraph/sourcegraph/internal/search"
	"github.com/sourcegraph/sourcegraph/internal/search/job"
	"github.com/sourcegraph/sourcegraph/internal/search/streaming"
)

func NewFileHasContributorsJob(child job.Job, includeOwners, excludeOwners []string) job.Job {
	return &fileHasContributorsJob{
		child:         child,
		includeOwners: includeOwners,
		excludeOwners: excludeOwners,
	}
}

type fileHasContributorsJob struct {
	child job.Job

	includeOwners []string
	excludeOwners []string
}

func (j *fileHasContributorsJob) Run(ctx context.Context, clients job.RuntimeClients, stream streaming.Sender) (alert *search.Alert, err error) {
	_, ctx, stream, finish := job.StartSpan(ctx, stream, j)
	defer finish(alert, err)

	filteredStream := streaming.StreamFunc(func(event streaming.SearchEvent) {

		//
		//event = j.filterEvent(ctx, clients.SearcherURLs, event)
		//
		filtered := event.Results[:0]
		for _, res := range event.Results {
			if fm, ok := res.(*result.FileMatch); ok {
				opts := gitserver.ContributorOptions{
					Range: string(fm.CommitID),
					Path:  fm.Path,
				}
				clients.Gitserver.ContributorCount(ctx, fm.Repo.Name, opts)

			}
		}
		// for _, res := range event.Results {
		//	switch v := res.(type) {
		//	case *result.FileMatch:
		//		filtered = append(filtered, j.filterFileMatch(v))
		//	case *result.CommitMatch:
		//		cm := j.filterCommitMatch(ctx, searcherURLs, v)
		//		if cm != nil {
		//			filtered = append(filtered, cm)
		//		}
		//	default:
		//		// Filter out any results that are not FileMatch or CommitMatch
		//	}
		//}
		event.Results = filtered

		stream.Send(event)
	})

	return j.child.Run(ctx, clients, filteredStream)
}

//func (j *fileContainsFilterJob) PartitionOwnerFormat

func (j *fileHasContributorsJob) MapChildren(fn job.MapFunc) job.Job {
	cp := *j
	cp.child = job.Map(j.child, fn)
	return &cp
}

func (j *fileHasContributorsJob) Name() string {
	return "FileHasContributorsFilterJob"
}

func (j *fileHasContributorsJob) Children() []job.Describer {
	return []job.Describer{j.child}
}

func (j *fileHasContributorsJob) Attributes(v job.Verbosity) (res []attribute.KeyValue) {
	switch v {
	case job.VerbosityMax:
		fallthrough
	case job.VerbosityBasic:
		res = append(res,
			attribute.StringSlice("includeOwners", j.includeOwners),
			attribute.StringSlice("excludeOwners", j.excludeOwners),
		)
	}
	return res
}
