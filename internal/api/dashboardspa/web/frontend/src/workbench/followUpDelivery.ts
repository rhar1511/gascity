import { supervisorApi } from '../supervisor/client';

export type FollowUpMailStatus =
  | 'not_submitted'
  | 'submitted'
  | 'accepted'
  | 'read'
  | 'failed';

export type FollowUpEventStreamStatus = 'connecting' | 'available' | 'timed_out' | 'unavailable';

export interface FollowUpDeliveryState {
  mail: FollowUpMailStatus;
  acknowledgement: 'unavailable';
  acknowledgementReason: 'verified_session_signal_missing';
  eventStream: FollowUpEventStreamStatus;
  messageId?: string;
}

export interface FollowUpEventStream extends EventTarget {
  onopen: ((event: Event) => void) | null;
  onerror: ((event: Event) => void) | null;
  close(): void;
}

export interface StartFollowUpDeliveryOptions {
  cityName: string;
  send: () => Promise<{ id: string }>;
  onChange: (state: FollowUpDeliveryState) => void;
  createEventSource?: (url: string) => FollowUpEventStream;
  mailReadTimeoutMs?: number;
  streamOpenTimeoutMs?: number;
}

const DEFAULT_STREAM_OPEN_TIMEOUT_MS = 2_000;
const DEFAULT_MAIL_READ_TIMEOUT_MS = 60_000;

/**
 * Submit mail after opening the event stream, then track only exact-message
 * mail.read events. Gas City currently has no verified active-session
 * acknowledgement signal, so this helper never promotes a read event to a
 * session acknowledgement.
 */
export function startFollowUpDelivery(options: StartFollowUpDeliveryOptions): () => void {
  let state: FollowUpDeliveryState = {
    mail: 'not_submitted',
    acknowledgement: 'unavailable',
    acknowledgementReason: 'verified_session_signal_missing',
    eventStream: 'connecting',
  };
  let source: FollowUpEventStream | null = null;
  let submitStarted = false;
  let stopped = false;
  let openTimer: ReturnType<typeof setTimeout> | null = null;
  let mailReadTimer: ReturnType<typeof setTimeout> | null = null;
  const earlyReadMessageIDs = new Set<string>();

  const publish = () => options.onChange({ ...state });
  const clearOpenTimer = () => {
    if (openTimer !== null) clearTimeout(openTimer);
    openTimer = null;
  };
  const clearMailReadTimer = () => {
    if (mailReadTimer !== null) clearTimeout(mailReadTimer);
    mailReadTimer = null;
  };
  const closeSource = () => {
    source?.close();
    source = null;
  };

  const submit = () => {
    if (stopped || submitStarted) return;
    submitStarted = true;
    state = { ...state, mail: 'submitted' };
    publish();
    let sendResult: Promise<{ id: string }>;
    try {
      sendResult = options.send();
    } catch {
      state = { ...state, mail: 'failed', eventStream: 'unavailable' };
      closeSource();
      publish();
      return;
    }
    void sendResult.then(
      (message) => {
        if (stopped) return;
        const wasRead = message.id.length > 0 && earlyReadMessageIDs.has(message.id);
        state = {
          ...state,
          mail: wasRead ? 'read' : 'accepted',
          ...(message.id.length > 0 ? { messageId: message.id } : {}),
        };
        if (wasRead) {
          clearMailReadTimer();
          closeSource();
        } else if (message.id.length === 0) {
          state = { ...state, eventStream: 'unavailable' };
          closeSource();
        } else if (state.eventStream === 'available') {
          mailReadTimer = setTimeout(() => {
            if (stopped || state.mail !== 'accepted') return;
            clearMailReadTimer();
            state = { ...state, eventStream: 'timed_out' };
            closeSource();
            publish();
          }, options.mailReadTimeoutMs ?? DEFAULT_MAIL_READ_TIMEOUT_MS);
        }
        publish();
      },
      () => {
        if (stopped) return;
        state = { ...state, mail: 'failed', eventStream: 'unavailable' };
        clearMailReadTimer();
        closeSource();
        publish();
      },
    );
  };

  const markEventStreamUnavailable = () => {
    if (stopped || state.eventStream === 'unavailable') return;
    clearOpenTimer();
    clearMailReadTimer();
    state = { ...state, eventStream: 'unavailable' };
    closeSource();
    publish();
    submit();
  };

  const handleEvent = (event: Event) => {
    if (stopped) return;
    const messageID = mailReadMessageID(event);
    if (messageID === null) return;
    if (state.mail === 'accepted' && state.messageId === messageID) {
      clearMailReadTimer();
      state = { ...state, mail: 'read' };
      closeSource();
      publish();
    } else if (state.mail === 'submitted') {
      earlyReadMessageIDs.add(messageID);
    }
  };

  publish();
  if (
    options.cityName.trim().length === 0 ||
    (options.createEventSource === undefined && typeof globalThis.EventSource !== 'function')
  ) {
    markEventStreamUnavailable();
  } else {
    try {
      const createEventSource =
        options.createEventSource ?? ((url: string) => new EventSource(url));
      const eventStreamUrl = supervisorApi().cityEventStreamUrl(options.cityName);
      source = createEventSource(eventStreamUrl);
      source.onopen = () => {
        if (stopped || state.eventStream !== 'connecting') return;
        clearOpenTimer();
        state = { ...state, eventStream: 'available' };
        publish();
        submit();
      };
      source.onerror = markEventStreamUnavailable;
      source.addEventListener('event', handleEvent);
      openTimer = setTimeout(
        markEventStreamUnavailable,
        options.streamOpenTimeoutMs ?? DEFAULT_STREAM_OPEN_TIMEOUT_MS,
      );
    } catch {
      markEventStreamUnavailable();
    }
  }

  return () => {
    if (stopped) return;
    stopped = true;
    clearOpenTimer();
    clearMailReadTimer();
    closeSource();
  };
}

function mailReadMessageID(event: Event): string | null {
  if (!(event instanceof MessageEvent) || typeof event.data !== 'string') return null;
  try {
    const value: unknown = JSON.parse(event.data);
    if (
      !isRecord(value) ||
      value.type !== 'mail.read' ||
      typeof value.subject !== 'string' ||
      value.subject.length === 0
    ) {
      return null;
    }
    return value.subject;
  } catch {
    return null;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}
