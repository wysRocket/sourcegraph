package codenav

import (
	"context"
	"strings"

	"github.com/sourcegraph/scip/bindings/go/scip"
	"go.opentelemetry.io/otel/attribute"

	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/codenav/shared"
	"github.com/sourcegraph/sourcegraph/internal/observation"
	"github.com/sourcegraph/sourcegraph/lib/codeintel/precise"
)

func (s *Service) NewGetDefinitions(ctx context.Context, args RequestArgs, requestState RequestState) (_ []shared.UploadLocation, err error) {
	locations, _, err := s.gatherLocations(
		ctx,
		args,
		requestState,
		s.operations.getDefinitions,
		GenericCursor{},
		"definitions",
		true,
		s.makeDefinitionUploadFactory(requestState),
		s.lsifstore.ExtractDefinitionLocationsFromPosition,
	)

	return locations, err
}

func (s *Service) NewGetReferences(ctx context.Context, args RequestArgs, requestState RequestState, cursor GenericCursor) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	return s.gatherLocations(
		ctx,
		args,
		requestState,
		s.operations.getReferences,
		cursor,
		"references",
		false,
		s.makeReferencesUploadFactory(args, requestState),
		s.lsifstore.ExtractReferenceLocationsFromPosition,
	)
}

func (s *Service) NewGetImplementations(ctx context.Context, args RequestArgs, requestState RequestState, cursor GenericCursor) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	return s.gatherLocations(
		ctx,
		args,
		requestState,
		s.operations.getImplementations,
		cursor,
		"implementations",
		false,
		s.makeReferencesUploadFactory(args, requestState),
		s.lsifstore.ExtractImplementationLocationsFromPosition,
	)
}

func (s *Service) NewGetPrototypes(ctx context.Context, args RequestArgs, requestState RequestState, cursor GenericCursor) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	return s.gatherLocations(
		ctx,
		args,
		requestState,
		s.operations.getPrototypes,
		cursor,
		"definitions", // N.B.
		false,
		s.makeDefinitionUploadFactory(requestState),
		s.lsifstore.ExtractPrototypeLocationsFromPosition,
	)
}

//
//

func (s *Service) makeDefinitionUploadFactory(requestState RequestState) getSearchableUploadIDsFunc {
	return func(ctx context.Context, monikers []precise.QualifiedMonikerData) ([]int, error) {
		uploads, err := s.getUploadsWithDefinitionsForMonikers(ctx, monikers, requestState)
		if err != nil {
			return nil, err
		}

		var ids []int
		for _, u := range uploads {
			ids = append(ids, u.ID)
		}
		return ids, nil
	}
}

func (s *Service) makeReferencesUploadFactory(args RequestArgs, requestState RequestState) getSearchableUploadIDsFunc {
	return func(ctx context.Context, monikers []precise.QualifiedMonikerData) ([]int, error) {
		uploads, err := s.getUploadsWithDefinitionsForMonikers(ctx, monikers, requestState)
		if err != nil {
			return nil, err
		}

		var ids []int
		for _, u := range uploads {
			ids = append(ids, u.ID)
		}

		referenceIDs, _, _, err := s.uploadSvc.GetUploadIDsWithReferences(
			ctx,
			monikers,
			ids,
			args.RepositoryID,
			args.Commit,
			requestState.maximumIndexesPerMonikerSearch,
			0, // offset
		)
		if err != nil {
			return nil, err
		}
		// Fetch the upload records we don't currently have hydrated and insert them into the map
		if _, err := s.getUploadsByIDs(ctx, referenceIDs, requestState); err != nil {
			return nil, err
		}

		return append(ids, referenceIDs...), nil
	}
}

//
//

type getSearchableUploadIDsFunc func(ctx context.Context, monikers []precise.QualifiedMonikerData) ([]int, error)
type getLocationsFromPositionFunc func(ctx context.Context, bundleID int, path string, line, character, limit, offset int) ([]shared.Location, int, []string, error)

const skipPrefix = "lsif ."

var exhaustedCursor = GenericCursor{Phase: "done"}

func (s *Service) gatherLocations(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	operation *observation.Operation,
	cursor GenericCursor,
	tableName string,
	stopAfterFirstResult bool,
	getSearchableUploadIDs getSearchableUploadIDsFunc,
	getLocationsFromPosition getLocationsFromPositionFunc,
) (_ []shared.UploadLocation, _ GenericCursor, err error) {
	ctx, trace, endObservation := observeResolver(ctx, &err, operation, serviceObserverThreshold, observation.Args{Attrs: []attribute.KeyValue{
		attribute.Int("repositoryID", args.RepositoryID),
		attribute.String("commit", args.Commit),
		attribute.String("path", args.Path),
		attribute.Int("numUploads", len(requestState.GetCacheUploads())),
		attribute.String("uploads", uploadIDsToString(requestState.GetCacheUploads())),
		attribute.Int("line", args.Line),
		attribute.Int("character", args.Character),
	}})
	defer endObservation()

	_ = trace // TODO - add tracing

	// PHASE 1:
	// Determine the set of visible uploads for the source commit
	visibleUploads, cursor, err := s.phase1(ctx, args, requestState, cursor)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	// PHASE 2:
	// Look in the user's current path/location in the visible uploads and pull
	// locations directly from that document. As a side-effect, we'll also gather
	// the set of relevant symbol names to search over remote indexes, depending
	// on the specific relationship being queried.
	localLocations, monikers, skipPaths, err := s.phase2(ctx, args, requestState, stopAfterFirstResult, getLocationsFromPosition, visibleUploads)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	// PHASE 3:
	// Determine the set of uploads that could have locations related to the given
	// set of target symbol names.
	uploadIDs, err := s.phase3(ctx, getSearchableUploadIDs, monikers)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	// PHASE 4:
	// Search in batches for target symbol names over our selected candidate indexes
	remoteLocations, err := s.phase4(ctx, args, requestState, tableName, monikers, skipPaths, uploadIDs)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	return append(localLocations, remoteLocations...), exhaustedCursor, nil
}

// WIP
func (s *Service) phase1(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
) ([]visibleUpload, GenericCursor, error) {
	visibleUploads, cursorsToVisibleUploads, err := s.getVisibleUploadsFromCursor(ctx, args.Line, args.Character, &cursor.CursorsToVisibleUploads, requestState)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	cursor.CursorsToVisibleUploads = cursorsToVisibleUploads
	return visibleUploads, cursor, nil
}

// WIP
func (s *Service) phase2(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	stopAfterFirstResult bool,
	getLocationsFromPosition getLocationsFromPositionFunc,
	visibleUploads []visibleUpload,
) ([]shared.UploadLocation, []precise.QualifiedMonikerData, map[int]string, error) {
	var (
		combinedLocations []shared.UploadLocation
		allSymbols        = map[string]struct{}{}
		skipPaths         = map[int]string{}
	)

	for i := range visibleUploads {
		// TODO - paginate
		locations, _, uploadSymbols, err := getLocationsFromPosition(
			ctx,
			visibleUploads[i].Upload.ID,
			visibleUploads[i].TargetPathWithoutRoot,
			visibleUploads[i].TargetPosition.Line,
			visibleUploads[i].TargetPosition.Character,
			args.Limit,
			0,
		)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(locations) > 0 {
			adjustedLocations, err := s.getUploadLocations(ctx, args, requestState, locations, true)
			if err != nil {
				return nil, nil, nil, err
			}
			if stopAfterFirstResult {
				return adjustedLocations, nil, nil, nil
			}

			combinedLocations = append(combinedLocations, adjustedLocations...)
			skipPaths[visibleUploads[i].Upload.ID] = visibleUploads[i].TargetPathWithoutRoot
		}

		for _, symbolName := range uploadSymbols {
			if !strings.HasPrefix(symbolName, skipPrefix) {
				allSymbols[symbolName] = struct{}{}
			}
		}
	}

	var symbolNames []string
	for symbolName := range allSymbols {
		symbolNames = append(symbolNames, symbolName)
	}

	monikers, err := symbolsToMonikers(symbolNames)
	if err != nil {
		return nil, nil, nil, err
	}

	return combinedLocations, monikers, skipPaths, nil
}

// WIP
func (s *Service) phase3(
	ctx context.Context,
	getSearchableUploadIDs getSearchableUploadIDsFunc,
	monikers []precise.QualifiedMonikerData,
) ([]int, error) {
	// TODO - paginate
	uploadIDs, err := getSearchableUploadIDs(ctx, monikers)
	if err != nil {
		return nil, err
	}

	return uploadIDs, nil
}

// WIP
func (s *Service) phase4(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	tableName string,
	monikers []precise.QualifiedMonikerData,
	skipPaths map[int]string,
	uploadIDs []int,
) ([]shared.UploadLocation, error) {
	monikerArgs := make([]precise.MonikerData, 0, len(monikers))
	for _, moniker := range monikers {
		monikerArgs = append(monikerArgs, moniker.MonikerData)
	}

	// TODO - paginate
	locations, _, err := s.lsifstore.GetMinimalBulkMonikerLocations(ctx, tableName, uploadIDs, skipPaths, monikerArgs, 10000, 0)
	if err != nil {
		return nil, err
	}

	// Adjust locations back to target commit
	return s.getUploadLocations(ctx, args, requestState, locations, false)
}

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
