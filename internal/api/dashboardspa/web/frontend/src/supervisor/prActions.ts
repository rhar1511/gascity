// Preserve the Workbench names while keeping the generated API as wire owner.
export type {
  PrActionSource as PRActionSource,
  PrActionQueue as PRActionQueue,
  PrActionQueueItem as PRActionQueueItem,
  PrActionWorkRecord as PRActionWorkRecord,
  PrActionAttemptReference as PRActionAttemptReference,
  PrActionOption as PRActionOption,
  PrActionResult as PRActionResult,
} from 'gas-city-dashboard-shared/gc-supervisor';

export type PRActionKind = 'prepare' | 'queue_review';
