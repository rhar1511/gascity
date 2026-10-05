export type PRActionKind = 'prepare' | 'queue_review';

export interface PRActionSource {
  monitor: string;
  owner: string;
  repo: string;
  rig: string;
  state: string;
  detail?: string;
}

export interface PRActionQueue {
  availability: string;
  policy_state: string;
  policy_detail?: string;
  policy_version: string;
  observed_at: string;
  fresh_until: string;
  sources?: PRActionSource[] | null;
  items?: PRActionQueueItem[] | null;
}

export interface PRActionQueueItem {
  monitor: string;
  owner: string;
  repo: string;
  pull_request: number;
  title: string;
  url?: string;
  base_ref_name: string;
  head_ref_name?: string;
  head_sha: string;
  base_sha: string;
  merge_state: string;
  is_draft: boolean;
  policy_version: string;
  observed_at: string;
  fresh_until: string;
  work_records?: PRActionWorkRecord[] | null;
  evidence_state: string;
  attempt_evidence?: PRActionAttemptReference[] | null;
  action_receipts?: PRActionResult[] | null;
  actions?: PRActionOption[] | null;
}

export interface PRActionWorkRecord {
  id: string;
  status: string;
  assignee?: string;
  candidate_sha: string;
  base_sha: string;
  current_revision: boolean;
}

export interface PRActionAttemptReference {
  store_ref: string;
  work_id: string;
  attempt_id: string;
  base_sha: string;
  candidate_sha: string;
  diff_sha256: string;
  diff_source: string;
  working_tree_status: string;
}

export interface PRActionOption {
  action: string;
  available: boolean;
  requires_human_approval: boolean;
  reason: string;
}

export interface PRActionResult {
  id: string;
  action: string;
  status: string;
  outcome?: string;
  detail?: string;
  idempotency_key: string;
  monitor: string;
  owner: string;
  repo: string;
  pull_request: number;
  work_id?: string;
  attempt_id?: string;
  head_sha: string;
  base_sha: string;
  policy_version: string;
  actor_key_id: string;
  created_at: string;
  verified_at?: string;
}

export interface ExecutePRActionRequest {
  action: PRActionKind;
  monitor: string;
  owner: string;
  repo: string;
  pull_request: number;
  head_sha: string;
  base_sha: string;
  policy_version: string;
  work_id?: string;
  attempt_id?: string;
  idempotency_key: string;
}
