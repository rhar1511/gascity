import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  startFollowUpDelivery,
  type FollowUpDeliveryState,
  type FollowUpEventStream,
} from './followUpDelivery';

class FakeEventSource extends EventTarget implements FollowUpEventStream {
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  close = vi.fn();

  open() {
    this.onopen?.(new Event('open'));
  }

  fail() {
    this.onerror?.(new Event('error'));
  }

  emit(data: unknown) {
    this.dispatchEvent(new MessageEvent('event', { data: JSON.stringify(data) }));
  }
}

function latestState(states: FollowUpDeliveryState[]): FollowUpDeliveryState {
  const state = states.at(-1);
  if (!state) throw new Error('expected a follow-up state');
  return state;
}

describe('follow-up delivery acknowledgement', () => {
  afterEach(() => vi.useRealTimers());

  it('subscribes before sending and reports exact-message mail read without session acknowledgement', async () => {
    const source = new FakeEventSource();
    const states: FollowUpDeliveryState[] = [];
    const send = vi.fn(async () => ({ id: 'mail-9' }));

    startFollowUpDelivery({
      cityName: 'test-city',
      send,
      onChange: (state) => states.push(state),
      createEventSource: (url) => {
        expect(url).toContain('/v0/city/test-city/events/stream');
        return source;
      },
    });

    expect(send).not.toHaveBeenCalled();
    source.open();
    await Promise.resolve();
    expect(send).toHaveBeenCalledOnce();
    expect(latestState(states)).toMatchObject({
      mail: 'accepted',
      acknowledgement: 'unavailable',
      acknowledgementReason: 'verified_session_signal_missing',
    });

    source.emit({ type: 'mail.read', subject: 'another-mail', actor: 'session-7' });
    source.emit({ type: 'session.output', subject: 'mail-9', actor: 'session-7' });
    expect(latestState(states).mail).toBe('accepted');

    source.emit({
      type: 'mail.read',
      subject: 'mail-9',
      actor: 'session-7',
      session_id: 'session-7',
    });
    expect(latestState(states)).toMatchObject({
      mail: 'read',
      acknowledgement: 'unavailable',
      acknowledgementReason: 'verified_session_signal_missing',
    });
    expect(source.close).toHaveBeenCalledOnce();
  });

  it('buffers mail-read events until the send response identifies the exact message', async () => {
    const source = new FakeEventSource();
    const states: FollowUpDeliveryState[] = [];
    let resolveSend!: (message: { id: string }) => void;
    const send = vi.fn(
      () => new Promise<{ id: string }>((resolve) => (resolveSend = resolve)),
    );

    startFollowUpDelivery({
      cityName: 'test-city',
      send,
      onChange: (state) => states.push(state),
      createEventSource: () => source,
    });
    source.open();
    source.emit({ type: 'mail.read', subject: 'mail-9', actor: 'another-actor' });

    expect(latestState(states).mail).toBe('submitted');
    resolveSend({ id: 'mail-9' });
    await Promise.resolve();
    expect(latestState(states)).toMatchObject({
      mail: 'read',
      acknowledgement: 'unavailable',
      acknowledgementReason: 'verified_session_signal_missing',
    });
  });

  it('reports unavailable event monitoring but still reports mail acceptance', async () => {
    const source = new FakeEventSource();
    const states: FollowUpDeliveryState[] = [];

    startFollowUpDelivery({
      cityName: 'test-city',
      send: async () => ({ id: 'mail-9' }),
      onChange: (state) => states.push(state),
      createEventSource: () => source,
    });
    source.fail();
    await Promise.resolve();

    expect(latestState(states)).toMatchObject({
      mail: 'accepted',
      acknowledgement: 'unavailable',
      eventStream: 'unavailable',
    });
  });

  it('keeps a rejected mail submission separate from acceptance and read', async () => {
    const source = new FakeEventSource();
    const states: FollowUpDeliveryState[] = [];

    startFollowUpDelivery({
      cityName: 'test-city',
      send: async () => Promise.reject(new Error('mail unavailable')),
      onChange: (state) => states.push(state),
      createEventSource: () => source,
    });
    source.open();
    await vi.waitFor(() => {
      expect(latestState(states)).toMatchObject({
        mail: 'failed',
        acknowledgement: 'unavailable',
        eventStream: 'unavailable',
      });
    });
    expect(source.close).toHaveBeenCalledOnce();
  });

  it('times out connecting to the event stream but still submits without claiming acknowledgement', async () => {
    vi.useFakeTimers();
    const source = new FakeEventSource();
    const states: FollowUpDeliveryState[] = [];

    startFollowUpDelivery({
      cityName: 'test-city',
      send: async () => ({ id: 'mail-9' }),
      onChange: (state) => states.push(state),
      createEventSource: () => source,
      streamOpenTimeoutMs: 500,
    });
    await vi.advanceTimersByTimeAsync(500);
    await Promise.resolve();

    expect(latestState(states)).toMatchObject({
      mail: 'accepted',
      acknowledgement: 'unavailable',
      eventStream: 'unavailable',
    });
  });

  it('times out waiting for a mail-read event while keeping session acknowledgement unavailable', async () => {
    vi.useFakeTimers();
    const source = new FakeEventSource();
    const states: FollowUpDeliveryState[] = [];

    startFollowUpDelivery({
      cityName: 'test-city',
      send: async () => ({ id: 'mail-9' }),
      onChange: (state) => states.push(state),
      createEventSource: () => source,
      mailReadTimeoutMs: 500,
    });
    source.open();
    await Promise.resolve();
    await vi.advanceTimersByTimeAsync(500);

    expect(latestState(states)).toMatchObject({
      mail: 'accepted',
      acknowledgement: 'unavailable',
      eventStream: 'timed_out',
    });
  });
});
