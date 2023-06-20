package codenav

import (
	"context"

	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/codenav/shared"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

func (s *Service) NewGetDefinitions(ctx context.Context, args RequestArgs, requestState RequestState) (_ []shared.UploadLocation, err error) {
	// TODO
	return nil, errors.New("unimplemented")
}

func (s *Service) NewGetReferences(ctx context.Context, args RequestArgs, requestState RequestState, cursor GenericCursor) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	// TODO
	return nil, GenericCursor{}, errors.New("unimplemented")
}

func (s *Service) NewGetImplementations(ctx context.Context, args RequestArgs, requestState RequestState, cursor GenericCursor) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	// TODO
	return nil, GenericCursor{}, errors.New("unimplemented")
}

func (s *Service) NewGetPrototypes(ctx context.Context, args RequestArgs, requestState RequestState, cursor GenericCursor) (_ []shared.UploadLocation, nextCursor GenericCursor, err error) {
	// TODO
	return nil, GenericCursor{}, errors.New("unimplemented")
}
