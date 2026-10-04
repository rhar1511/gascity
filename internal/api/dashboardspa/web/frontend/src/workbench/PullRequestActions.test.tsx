import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { PRActionOption, PRActionQueueItem } from '../supervisor/prActions';
import { PullRequestActions } from './PullRequestActions';

const item: PRActionQueueItem = {
  monitor: 'main',
  owner: 'acme',
  repo: 'widget',
  pull_request: 42,
  title: 'Add a feature',
  url: 'https://github.com/acme/widget/pull/42',
  base_ref_name: 'main',
  head_ref_name: 'work-1',
  head_sha: 'a'.repeat(40),
  base_sha: 'b'.repeat(40),
  merge_state: 'CLEAN',
  is_draft: false,
  policy_version: 'policy-v1',
  observed_at: '2026-09-29T00:00:00Z',
  fresh_until: '2026-09-29T00:00:30Z',
  evidence_state: 'verified',
  work_records: null,
  attempt_evidence: null,
  action_receipts: null,
  actions: [],
};

const allowed: PRActionOption = {
  action: 'prepare',
  available: true,
  requires_human_approval: false,
  reason: 'server verdict allows prepare',
};

afterEach(cleanup);

describe('PullRequestActions', () => {
  it('renders server-provided actions and never offers merge or force push', () => {
    render(
      <PullRequestActions
        item={{ ...item, actions: [allowed] }}
        prepare={allowed}
        queueReview={{ ...allowed, action: 'queue_review' }}
        busy={null}
        refreshing={false}
        actionsDisabled={false}
        error={null}
        receipt={null}
        onAction={vi.fn()}
        onRefresh={vi.fn()}
      />,
    );

    expect(screen.getByRole('button', { name: /prepare pr/i })).toBeTruthy();
    expect(screen.getByRole('button', { name: /queue pr for review/i })).toBeTruthy();
    expect(screen.getByRole('link', { name: /open pull request/i }).getAttribute('href')).toBe(
      item.url,
    );
    expect(screen.queryByRole('button', { name: /merge|force/i })).toBeNull();
  });

  it('keeps server-rejected actions visible but disabled with the server reason', () => {
    render(
      <PullRequestActions
        item={item}
        prepare={{ ...allowed, available: false, reason: 'merge conflict' }}
        queueReview={null}
        busy={null}
        refreshing={false}
        actionsDisabled={false}
        error={null}
        receipt={null}
        onAction={vi.fn()}
        onRefresh={vi.fn()}
      />,
    );

    expect(screen.getByRole('button', { name: /prepare pr/i }).hasAttribute('disabled')).toBe(true);
    expect(screen.getByText(/prepare: merge conflict/i)).toBeTruthy();
    expect(screen.queryByRole('button', { name: /merge/i })).toBeNull();
  });

  it('fails closed on a cached verdict after queue refresh fails but leaves refresh available', () => {
    const onRefresh = vi.fn();
    render(
      <PullRequestActions
        item={item}
        prepare={allowed}
        queueReview={null}
        busy={null}
        refreshing={false}
        actionsDisabled
        error="queue temporarily unavailable"
        receipt={null}
        onAction={vi.fn()}
        onRefresh={onRefresh}
      />,
    );

    expect(screen.getByRole('button', { name: /prepare pr/i }).hasAttribute('disabled')).toBe(true);
    const refresh = screen.getByRole('button', { name: /refresh pr verdict/i });
    expect((refresh as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(refresh);
    expect(onRefresh).toHaveBeenCalledOnce();
  });

  it('explains when Gas City offers no action for this revision', () => {
    render(
      <PullRequestActions
        item={item}
        prepare={null}
        queueReview={null}
        busy={null}
        refreshing={false}
        actionsDisabled={false}
        error={null}
        receipt={null}
        onAction={vi.fn()}
        onRefresh={vi.fn()}
      />,
    );

    expect(screen.getByRole('status').textContent).toMatch(
      /offers no prepare or review-queue action/i,
    );
  });

  it('does not render unsafe pull-request URLs as links', () => {
    render(
      <PullRequestActions
        item={{ ...item, url: 'javascript:alert(1)' }}
        prepare={null}
        queueReview={null}
        busy={null}
        refreshing={false}
        actionsDisabled={false}
        error={null}
        receipt={null}
        onAction={vi.fn()}
        onRefresh={vi.fn()}
      />,
    );

    expect(screen.queryByRole('link', { name: /open pull request/i })).toBeNull();
  });
});
