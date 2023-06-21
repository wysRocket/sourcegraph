package codenav

import (
	"context"
	"sort"
	"strings"

	"github.com/sourcegraph/scip/bindings/go/scip"
	"go.opentelemetry.io/otel/attribute"

	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/codenav/shared"
	"github.com/sourcegraph/sourcegraph/internal/collections"
	"github.com/sourcegraph/sourcegraph/internal/observation"
	"github.com/sourcegraph/sourcegraph/lib/codeintel/precise"
)

func (s *Service) NewGetDefinitions(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
) (_ []shared.UploadLocation, err error) {
	locations, _, err := s.gatherLocations(
		ctx, args, requestState, GenericCursor{},

		s.operations.getDefinitions, // operation
		"definitions",               // tableName
		false,                       // includeReferencingIndexes
		s.lsifstore.ExtractDefinitionLocationsFromPosition,
	)

	return locations, err
}

func (s *Service) NewGetReferences(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	return s.gatherLocations(
		ctx, args, requestState, cursor,

		s.operations.getReferences, // operation
		"references",               // tableName
		true,                       // includeReferencingIndexes
		s.lsifstore.ExtractReferenceLocationsFromPosition,
	)
}

func (s *Service) NewGetImplementations(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	return s.gatherLocations(
		ctx, args, requestState, cursor,

		s.operations.getImplementations, // operation
		"implementations",               // tableName
		true,                            // includeReferencingIndexes
		s.lsifstore.ExtractImplementationLocationsFromPosition,
	)
}

func (s *Service) NewGetPrototypes(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	return s.gatherLocations(
		ctx, args, requestState, cursor,

		s.operations.getPrototypes, // operation
		"definitions",              // N.B.: TODO
		false,                      // includeReferencingIndexes
		s.lsifstore.ExtractPrototypeLocationsFromPosition,
	)
}

//
//

type extractPrototypeLocationsFromPositionFunc func(
	ctx context.Context,
	bundleID int,
	path string,
	line int,
	character int,
	limit int,
	offset int,
) ([]shared.Location, int, []string, error)

func (s *Service) gatherLocations(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
	operation *observation.Operation,
	tableName string,
	includeReferencingIndexes bool,
	extractPrototypeLocationsFromPosition extractPrototypeLocationsFromPositionFunc,
) (allLocations []shared.UploadLocation, _ GenericCursor, err error) {
	// TODO - update, add trace logs below
	ctx, _, endObservation := observeResolver(ctx, &err, operation, serviceObserverThreshold, observation.Args{Attrs: []attribute.KeyValue{
		attribute.Int("repositoryID", args.RepositoryID),
		attribute.String("commit", args.Commit),
		attribute.String("path", args.Path),
		attribute.Int("numUploads", len(requestState.GetCacheUploads())),
		attribute.String("uploads", uploadIDsToString(requestState.GetCacheUploads())),
		attribute.Int("line", args.Line),
		attribute.Int("character", args.Character),
	}})
	defer endObservation()

	if cursor.Phase == "" {
		cursor.Phase = "local"
	}

	// First, we determine the set of SCIP indexes that can act as one of our "roots" for the
	// following traversal. We see which SCIP indexes cover the particular query position and
	// stash this metadata on the cursor for subsequent queries.

	var visibleUploads []visibleUpload

	// N.B.: cursor is purposefully re-assigned here
	visibleUploads, cursor, err = s.newGetVisibleUploadsFromCursor(
		ctx,
		args,
		requestState,
		cursor,
	)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	// The following loop calls local and remote location resolution phases in alternation. As
	// each phase controls whether or not it should execute, this is safe.
	//
	// Such a loop exists as each invocation of either phase may produce fewer results than the
	// requested page size. For example, the local phase may have a small number of results but
	// the remote phase has additional results that could fit on the first page. Similarly, if
	// there are many references to a symbol over a large number of indexes but each index has
	// only a small number of locations, they can all be combined into a single page. Running
	// each phase multiple times and combining the results will create a full page, if the
	// result set was not exhausted),on each round-trip call to this service's method.

outer:
	for cursor.Phase != "done" {
		for _, gatherLocations := range []gatherLocationsFunc{s.gatherLocalLocations, s.gatherRemoteLocations} {
			if len(allLocations) >= args.Limit {
				// we've filled our page, exit with current results
				break outer
			}

			var locations []shared.UploadLocation

			// N.B.: cursor is purposefully re-assigned here
			locations, cursor, err = gatherLocations(
				ctx,
				args,
				requestState,
				cursor,
				tableName,
				extractPrototypeLocationsFromPosition,
				includeReferencingIndexes,
				visibleUploads,
				args.Limit-len(allLocations), // remaining space in the page
			)
			if err != nil {
				return nil, GenericCursor{}, err
			}
			allLocations = append(allLocations, locations...)
		}
	}

	return allLocations, cursor, nil
}

func (s *Service) newGetVisibleUploadsFromCursor(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
) ([]visibleUpload, GenericCursor, error) {
	if cursor.VisibleUploads != nil {
		visibleUploads := make([]visibleUpload, 0, len(cursor.VisibleUploads))
		for _, u := range cursor.VisibleUploads {
			upload, ok := requestState.dataLoader.GetUploadFromCacheMap(u.DumpID)
			if !ok {
				return nil, GenericCursor{}, ErrConcurrentModification
			}

			visibleUploads = append(visibleUploads, visibleUpload{
				Upload:                upload,
				TargetPath:            u.TargetPath,
				TargetPosition:        u.TargetPosition,
				TargetPathWithoutRoot: u.TargetPathWithoutRoot,
			})
		}

		return visibleUploads, cursor, nil
	}

	visibleUploads, err := s.getVisibleUploads(ctx, args.Line, args.Character, requestState)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	cursorVisibleUpload := make([]CursorVisibleUpload, 0, len(visibleUploads))
	for i := range visibleUploads {
		cursorVisibleUpload = append(cursorVisibleUpload, CursorVisibleUpload{
			DumpID:                visibleUploads[i].Upload.ID,
			TargetPath:            visibleUploads[i].TargetPath,
			TargetPosition:        visibleUploads[i].TargetPosition,
			TargetPathWithoutRoot: visibleUploads[i].TargetPathWithoutRoot,
		})
	}

	cursor.VisibleUploads = cursorVisibleUpload
	return visibleUploads, cursor, nil
}

type gatherLocationsFunc func(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
	tableName string,
	getLocationsFromPosition extractPrototypeLocationsFromPositionFunc,
	includeReferencingIndexes bool,
	visibleUploads []visibleUpload,
	limit int,
) ([]shared.UploadLocation, GenericCursor, error)

const skipPrefix = "lsif ."

func (s *Service) gatherLocalLocations(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
	_ string,
	extractPrototypeLocationsFromPosition extractPrototypeLocationsFromPositionFunc,
	_ bool,
	visibleUploads []visibleUpload,
	limit int,
) (allLocations []shared.UploadLocation, _ GenericCursor, _ error) {
	if cursor.Phase != "local" {
		// not our turn
		return nil, cursor, nil
	}
	if cursor.LocalUploadOffset >= len(visibleUploads) {
		// nothing left to do
		cursor.Phase = "remote"
		return nil, cursor, nil
	}
	unconsumedVisibleUploads := visibleUploads[cursor.LocalUploadOffset:]

	// Create local copy of mutable cursor scope and normalize it before use.
	// We will re-assign these values back to the response cursor before the
	// function exits.
	allSymbolNames := collections.NewSet(cursor.SymbolNames...)
	skipPathsByUploadID := cursor.SkipPathsByUploadID

	if skipPathsByUploadID == nil {
		// prevent writes to nil map
		skipPathsByUploadID = map[int]string{}
	}

	for _, visibleUpload := range unconsumedVisibleUploads {
		if len(allLocations) >= limit {
			// break if we've already hit our page maximum
			break
		}

		// Gather response locations directly from the document containing the
		// target position. This may also return relevant symbol names that we
		// collect for a remote search.
		locations, totalCount, symbolNames, err := extractPrototypeLocationsFromPosition(
			ctx,
			visibleUpload.Upload.ID,
			visibleUpload.TargetPathWithoutRoot,
			visibleUpload.TargetPosition.Line,
			visibleUpload.TargetPosition.Character,
			limit-len(allLocations), // remaining space in the page
			cursor.LocalLocationOffset,
		)
		if err != nil {
			return nil, GenericCursor{}, err
		}

		// adjust cursor offset for next page
		cursor.LocalLocationOffset += len(locations)
		if cursor.LocalLocationOffset >= totalCount {
			// We've consumed this upload completely. Skip it the next time we find
			// ourselves in this loop, and ensure that we start with a zero offset on
			// the next upload we process (if any).
			cursor.LocalUploadOffset++
			cursor.LocalLocationOffset = 0
		}

		// consume locations
		if len(locations) > 0 {
			adjustedLocations, err := s.getUploadLocations(
				ctx,
				args,
				requestState,
				locations,
				true,
			)
			if err != nil {
				return nil, GenericCursor{}, err
			}
			allLocations = append(allLocations, adjustedLocations...)

			// Stash paths with non-empty locations in the cursor so we can prevent
			// local and "remote" searches from returning duplicate sets of of target
			// ranges.
			skipPathsByUploadID[visibleUpload.Upload.ID] = visibleUpload.TargetPathWithoutRoot
		}

		// stash relevant symbol names in cursor
		for _, symbolName := range symbolNames {
			if !strings.HasPrefix(symbolName, skipPrefix) {
				allSymbolNames.Add(symbolName)
			}
		}
	}

	// re-assign mutable cursor scope to response cursor
	cursor.SymbolNames = allSymbolNames.Sorted(compareStrings)
	cursor.SkipPathsByUploadID = skipPathsByUploadID

	return allLocations, cursor, nil
}

func (s *Service) gatherRemoteLocations(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
	tableName string,
	_ extractPrototypeLocationsFromPositionFunc,
	includeReferencingIndexes bool,
	_ []visibleUpload,
	limit int,
) ([]shared.UploadLocation, GenericCursor, error) {
	if cursor.Phase != "remote" {
		// not our turn
		return nil, cursor, nil
	}
	if len(cursor.SymbolNames) == 0 {
		// no symbol names from local phase
		return nil, exhaustedCursor, nil
	}

	monikers, err := symbolsToMonikers(cursor.SymbolNames)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	// Ensure we have a batch of upload ids over which to perform a symbol search, if such
	// a batch exists. This batch must be hydrated in the associated request data loader.
	// See the function body for additional complaints on this subject.
	//
	// N.B.: cursor is purposefully re-assigned here
	cursor, err = s.prepareCandidateUploads(
		ctx,
		args,
		requestState,
		cursor,
		includeReferencingIndexes,
		monikers,
	)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	// If we have no upload ids stashed in our cursor at this point then there are no more
	// uploads to search in and we've reached the end of our our result set. Congratulations!
	if len(cursor.UploadIDs) == 0 {
		return nil, exhaustedCursor, nil
	}

	// Finally, query time!
	// Fetch indexed ranges of the given symbols within the given uploads.

	monikerArgs := make([]precise.MonikerData, 0, len(monikers))
	for _, moniker := range monikers {
		monikerArgs = append(monikerArgs, moniker.MonikerData)
	}
	locations, totalCount, err := s.lsifstore.GetMinimalBulkMonikerLocations(
		ctx,
		tableName,
		cursor.UploadIDs,
		cursor.SkipPathsByUploadID,
		monikerArgs,
		limit,
		cursor.RemoteLocationOffset,
	)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	// adjust cursor offset for next page
	cursor.RemoteLocationOffset += len(locations)
	if cursor.RemoteLocationOffset >= totalCount {
		// We've consumed the locations for this set of uploads. Reset this slice value in the
		// cursor so that the next call to this function will query the new set of uploads to
		// search in while resolving the next page.We also ensure we start on a zero offset
		// for the next page of results for a fresh set of uploads (if any).
		cursor.UploadIDs = nil
		cursor.RemoteLocationOffset = 0
	}

	// Adjust locations back to target commit
	adjustedLocations, err := s.getUploadLocations(ctx, args, requestState, locations, false)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	return adjustedLocations, cursor, nil
}

func (s *Service) prepareCandidateUploads(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
	includeReferencingIndexes bool,
	monikers []precise.QualifiedMonikerData,
) (GenericCursor, error) {
	// We always want to look into the uploads that define one of the symbols for our
	// "remote" phase. We'll conditionally also look at uploads that contain only a
	// reference (see below). We deal with the former set of uploads first in the
	// cursor.

	if len(cursor.DefinitionIDs) == 0 && len(cursor.UploadIDs) == 0 && cursor.RemoteUploadOffset == 0 {
		// N.B.: We only end up in in this branch on the first time it's invoked while
		// in the remote phase. If there truly are no definitions, we'll either have a
		// non-empty set of upload ids, or a non-zero remote upload offset on the next
		// invocation. If there are neither definitions nor an upload batch, we'll end
		// up returning an exhausted cursor from _this_ invocation.

		uploads, err := s.getUploadsWithDefinitionsForMonikers(ctx, monikers, requestState)
		if err != nil {
			return GenericCursor{}, err
		}
		var ids []int
		for _, upload := range uploads {
			ids = append(ids, upload.ID)
		}
		sort.Ints(ids)

		cursor.UploadIDs = ids
		cursor.DefinitionIDs = ids
	}

	if !includeReferencingIndexes {
		// This traversal isn't looking in uploads without definitions to one of the symbols
		return cursor, nil
	}

	// If we have no upload ids stashed in our cursor, then we'll try to fetch the next
	// batch of uploads in which we'll search for symbol names. If our remote upload offset
	// is set to -1 here, then it indicates the end of the set of relevant upload records.

	if len(cursor.UploadIDs) == 0 && cursor.RemoteUploadOffset != -1 {
		uploadIDs, _, totalCount, err := s.uploadSvc.GetUploadIDsWithReferences(
			ctx,
			monikers,
			cursor.DefinitionIDs,
			args.RepositoryID,
			args.Commit,
			requestState.maximumIndexesPerMonikerSearch, // limit
			cursor.RemoteUploadOffset,                   // offset
		)
		if err != nil {
			return GenericCursor{}, err
		}
		cursor.UploadIDs = uploadIDs

		// adjust cursor offset for next page
		cursor.RemoteUploadOffset += len(uploadIDs)
		if cursor.RemoteUploadOffset >= totalCount {
			// We've consumed all upload batches
			cursor.RemoteUploadOffset = -1
		}
	}

	// Hydrate upload records into the request state data loader. This must be called prior
	// to the invocation of getUploadLocation, which will silently throw out records belonging
	// to uploads that have not yet fetched from the database. We've assumed that the data loader
	// is consistently up-to-date with any extant upload identifier reference.
	//
	// FIXME: That's a dangerous design assumption we should get rid of.
	if _, err := s.getUploadsByIDs(ctx, cursor.UploadIDs, requestState); err != nil {
		return GenericCursor{}, err
	}

	return cursor, nil
}

//
//

func symbolsToMonikers(symbolNames []string) ([]precise.QualifiedMonikerData, error) {
	var monikers []precise.QualifiedMonikerData
	for _, symbolName := range symbolNames {
		parsedSymbol, err := scip.ParseSymbol(symbolName)
		if err != nil {
			return nil, err
		}

		monikers = append(monikers, precise.QualifiedMonikerData{
			MonikerData: precise.MonikerData{
				Scheme:     parsedSymbol.Scheme,
				Identifier: symbolName,
			},
			PackageInformationData: precise.PackageInformationData{
				Manager: parsedSymbol.Package.Manager,
				Name:    parsedSymbol.Package.Name,
				Version: parsedSymbol.Package.Version,
			},
		})
	}

	return monikers, nil
}

func compareStrings(a, b string) bool {
	return a < b
}
