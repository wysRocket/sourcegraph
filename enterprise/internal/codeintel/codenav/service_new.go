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
		s.makeDefinitionUploadFactory(requestState),
		s.lsifstore.ExtractPrototypeLocationsFromPosition,
	)
}

//
//

func (s *Service) makeDefinitionUploadFactory(requestState RequestState) getSearchableUploadIDsFunc {
	return func(ctx context.Context, monikers []precise.QualifiedMonikerData, limit, offset int) ([]int, int, error) {
		uploads, err := s.getUploadsWithDefinitionsForMonikers(ctx, monikers, requestState)
		if err != nil {
			return nil, 0, err
		}

		var ids []int
		for _, u := range uploads {
			ids = append(ids, u.ID)
		}

		return ids, len(ids), nil
	}
}

func (s *Service) makeReferencesUploadFactory(args RequestArgs, requestState RequestState) getSearchableUploadIDsFunc {
	return func(ctx context.Context, monikers []precise.QualifiedMonikerData, limit, offset int) ([]int, int, error) {
		uploads, err := s.getUploadsWithDefinitionsForMonikers(ctx, monikers, requestState)
		if err != nil {
			return nil, 0, err
		}

		var ids []int
		for _, u := range uploads {
			ids = append(ids, u.ID)
		}

		// TODO - paginate
		referenceIDs, _, totalCount, err := s.uploadSvc.GetUploadIDsWithReferences(
			ctx,
			monikers,
			ids,
			args.RepositoryID,
			args.Commit,
			10000, // limit
			0,     // offset
		)
		if err != nil {
			return nil, 0, err
		}
		// Fetch the upload records we don't currently have hydrated and insert them into the map
		if _, err := s.getUploadsByIDs(ctx, referenceIDs, requestState); err != nil {
			return nil, 0, err
		}

		return append(ids, referenceIDs...), len(ids) + totalCount, nil
	}
}

//
//

type getSearchableUploadIDsFunc func(ctx context.Context, monikers []precise.QualifiedMonikerData, limit, offset int) ([]int, int, error)
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
	visibleUploads, cursorsToVisibleUploads, err := s.getVisibleUploadsFromCursor(ctx, args.Line, args.Character, &cursor.CursorsToVisibleUploads, requestState)
	if err != nil {
		return nil, GenericCursor{}, err
	}
	cursor.CursorsToVisibleUploads = cursorsToVisibleUploads

	var locations []shared.UploadLocation
	var allLocations []shared.UploadLocation
	for cursor.Phase != "done" && len(allLocations) < args.Limit {
		locations, cursor, err = s.gatherLocalLocations(ctx, args, requestState, cursor, getLocationsFromPosition, visibleUploads, args.Limit-len(allLocations))
		if err != nil {
			return nil, GenericCursor{}, err
		}
		allLocations = append(allLocations, locations...)

		if len(allLocations) >= args.Limit {
			break
		}

		locations, cursor, err = s.gatherRemoteLocations(ctx, args, requestState, cursor, tableName, getSearchableUploadIDs, args.Limit-len(allLocations))
		if err != nil {
			return nil, GenericCursor{}, err
		}
		allLocations = append(allLocations, locations...)
	}

	return allLocations, cursor, nil
}

// WIP
func (s *Service) gatherLocalLocations(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
	getLocationsFromPosition getLocationsFromPositionFunc,
	visibleUploads []visibleUpload,
	limit int,
) ([]shared.UploadLocation, GenericCursor, error) {
	if cursor.Phase == "" {
		cursor.Phase = "local"
	}
	if cursor.Phase != "local" {
		return nil, cursor, nil
	}

	if cursor.LocalUploadOffset >= len(visibleUploads) {
		cursor.Phase = "remote"
		return nil, cursor, nil
	}

	var (
		combinedLocations []shared.UploadLocation
		allSymbols        = map[string]struct{}{}
		skipPaths         = cursor.SkipPaths
	)
	for _, s := range cursor.Symbols {
		allSymbols[s] = struct{}{}
	}
	if skipPaths == nil {
		skipPaths = map[int]string{}
	}

	for _, visibleUpload := range visibleUploads[cursor.LocalUploadOffset:] {
		if len(combinedLocations) >= limit {
			break
		}

		uploadID := visibleUpload.Upload.ID
		locations, totalCount, uploadSymbols, err := getLocationsFromPosition(
			ctx,
			uploadID,
			visibleUpload.TargetPathWithoutRoot,
			visibleUpload.TargetPosition.Line,
			visibleUpload.TargetPosition.Character,
			limit-len(combinedLocations),
			cursor.LocalLocationOffset,
		)
		if err != nil {
			return nil, GenericCursor{}, err
		}
		cursor.LocalLocationOffset += len(locations)
		if cursor.LocalLocationOffset >= totalCount {
			cursor.LocalUploadOffset++
			cursor.LocalLocationOffset = 0
		}

		if len(locations) > 0 {
			adjustedLocations, err := s.getUploadLocations(ctx, args, requestState, locations, true)
			if err != nil {
				return nil, GenericCursor{}, err
			}

			combinedLocations = append(combinedLocations, adjustedLocations...)
			skipPaths[uploadID] = visibleUpload.TargetPathWithoutRoot
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

	cursor.Symbols = symbolNames
	cursor.SkipPaths = skipPaths
	return combinedLocations, cursor, nil
}

// WIP
func (s *Service) gatherRemoteLocations(
	ctx context.Context,
	args RequestArgs,
	requestState RequestState,
	cursor GenericCursor,
	tableName string,
	getSearchableUploadIDs getSearchableUploadIDsFunc,
	limit int,
) ([]shared.UploadLocation, GenericCursor, error) {
	if cursor.Phase != "remote" {
		return nil, cursor, nil
	}
	if len(cursor.Symbols) == 0 {
		return nil, exhaustedCursor, nil
	}

	monikers, err := symbolsToMonikers(cursor.Symbols)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	// PHASE 3:
	// Determine the set of uploads that could have locations related to the given
	// set of target symbol names.

	if len(cursor.UploadIDs) == 0 && cursor.RemoteUploadOffset != -1 {
		uploadIDs, totalCount, err := getSearchableUploadIDs(ctx, monikers, requestState.maximumIndexesPerMonikerSearch, cursor.RemoteUploadOffset)
		if err != nil {
			return nil, GenericCursor{}, err
		}

		cursor.RemoteUploadOffset += len(uploadIDs)
		cursor.UploadIDs = append(cursor.UploadIDs, uploadIDs...)
		cursor.RemoteLocationOffset = 0

		if cursor.RemoteUploadOffset >= totalCount {
			cursor.RemoteUploadOffset = -1
		}
	}
	uploadIDs := cursor.UploadIDs

	if len(uploadIDs) == 0 {
		return nil, exhaustedCursor, nil
	}
	// Fetch the upload records we don't currently have hydrated and insert them into the map
	if _, err := s.getUploadsByIDs(ctx, uploadIDs, requestState); err != nil {
		return nil, GenericCursor{}, err
	}

	// PHASE 4:
	// Search in batches for target symbol names over our selected candidate indexes

	monikerArgs := make([]precise.MonikerData, 0, len(monikers))
	for _, moniker := range monikers {
		monikerArgs = append(monikerArgs, moniker.MonikerData)
	}

	locations, totalCount, err := s.lsifstore.GetMinimalBulkMonikerLocations(
		ctx,
		tableName,
		uploadIDs,
		cursor.SkipPaths,
		monikerArgs,
		limit,
		cursor.RemoteLocationOffset,
	)
	if err != nil {
		return nil, GenericCursor{}, err
	}
	cursor.RemoteLocationOffset += len(locations)
	if cursor.RemoteLocationOffset >= totalCount {
		cursor.UploadIDs = nil
	}

	// Adjust locations back to target commit
	adjustedLocations, err := s.getUploadLocations(ctx, args, requestState, locations, false)
	if err != nil {
		return nil, GenericCursor{}, err
	}

	return adjustedLocations, cursor, nil
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
