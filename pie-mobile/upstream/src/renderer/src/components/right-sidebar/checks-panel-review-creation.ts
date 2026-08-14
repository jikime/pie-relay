import type { HostedReviewCreationEligibility } from '../../../../shared/hosted-review'

type ReviewLookup = 'unknown' | 'positive_unresolved' | 'positive_resolved' | 'negative'
type MobileComposerEligibility = HostedReviewCreationEligibility & {
  reviewLookupOutcome?: 'found' | 'not_found' | 'unavailable'
}

/**
 * Extracted parity gate shared by the mobile PR form tests. It intentionally
 * fails closed whenever review existence is unresolved or unavailable, and
 * only opens for a confirmed create or push-and-create path.
 */
export function shouldOpenChecksPanelCreateComposer(input: {
  activeReview: unknown | null
  isFolder: boolean
  branch: string
  hostedReviewCreation: MobileComposerEligibility | null
  reviewLookup?: ReviewLookup
  hasHardRefreshError?: boolean
}): boolean {
  if (input.activeReview || input.isFolder || !input.branch) {
    return false
  }

  const eligibility = input.hostedReviewCreation
  if (!eligibility || eligibility.blockedReason === 'existing_review') {
    return false
  }
  if (input.reviewLookup === 'positive_unresolved' || input.hasHardRefreshError === true) {
    return false
  }
  if (eligibility.reviewLookupOutcome === 'unavailable') {
    return false
  }
  return eligibility.canCreate === true || eligibility.blockedReason === 'needs_push'
}
