package workflow

import (
	"context"
	"errors"

	"github.com/agenticworkflowdev/cli/internal/state"
)

type specificationResumer interface {
	Resume(context.Context, string, string) (SpecificationResult, error)
}

type implementationContinuation interface {
	ImplementationRunner
	Resume(context.Context, string, string) (ImplementationResult, error)
}

type reviewContinuation interface {
	Reviewer
	Resume(context.Context, string, string) (ReviewResult, error)
}

// ResumeContinuationService resumes one phase and then follows the normal
// downstream gates without reusing an earlier agent session.
type ResumeContinuationService struct {
	specification  specificationResumer
	implementation implementationContinuation
	review         reviewContinuation
}

// NewResumeContinuationService composes the three resumable phase services.
func NewResumeContinuationService(specification specificationResumer, implementation implementationContinuation, review reviewContinuation) *ResumeContinuationService {
	return &ResumeContinuationService{specification: specification, implementation: implementation, review: review}
}

// ContinueResume dispatches the recorded phase and preserves the normal
// specification -> implementation -> review ordering.
func (service *ResumeContinuationService) ContinueResume(ctx context.Context, controllerRoot, workflowID string, phase state.Phase) (ResumeContinuation, error) {
	if service == nil || service.specification == nil || service.implementation == nil || service.review == nil {
		return ResumeContinuation{}, errors.New("resume continuation service is not fully configured")
	}
	switch phase {
	case state.PhaseSpec:
		specification, err := service.specification.Resume(ctx, controllerRoot, workflowID)
		result := ResumeContinuation{Manifest: specification.Manifest, Specification: &specification, Blocker: specification.Blocker}
		if err != nil || specification.Blocker != nil {
			return result, err
		}
		implementation, err := service.implementation.Implement(ctx, controllerRoot, workflowID)
		result.Manifest = implementation.Manifest
		result.Implementation = &implementation
		result.Blocker = implementation.Blocker
		if err != nil || implementation.Blocker != nil {
			return result, err
		}
		reviewResult, err := service.review.Review(ctx, controllerRoot, workflowID, implementation.CheckResults, implementation.CheckedState)
		result.Manifest = reviewResult.Manifest
		result.Review = &reviewResult
		result.Blocker = reviewResult.Blocker
		return result, err

	case state.PhaseImplementation:
		implementation, err := service.implementation.Resume(ctx, controllerRoot, workflowID)
		result := ResumeContinuation{Manifest: implementation.Manifest, Implementation: &implementation, Blocker: implementation.Blocker}
		if err != nil || implementation.Blocker != nil {
			return result, err
		}
		reviewResult, err := service.review.Review(ctx, controllerRoot, workflowID, implementation.CheckResults, implementation.CheckedState)
		result.Manifest = reviewResult.Manifest
		result.Review = &reviewResult
		result.Blocker = reviewResult.Blocker
		return result, err

	case state.PhaseReview:
		reviewResult, err := service.review.Resume(ctx, controllerRoot, workflowID)
		return ResumeContinuation{Manifest: reviewResult.Manifest, Review: &reviewResult, Blocker: reviewResult.Blocker}, err
	default:
		return ResumeContinuation{}, errors.New("recorded workflow phase cannot be resumed")
	}
}
