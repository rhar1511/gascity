import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { FollowUpDeliveryStatus } from './FollowUpDeliveryStatus';

describe('FollowUpDeliveryStatus', () => {
  afterEach(cleanup);

  it('keeps accepted mail distinct from unavailable active-session acknowledgement', () => {
    render(
      <FollowUpDeliveryStatus
        state={{
          mail: 'accepted',
          acknowledgement: 'unavailable',
          acknowledgementReason: 'verified_session_signal_missing',
          eventStream: 'available',
          messageId: 'mail-9',
        }}
      />,
    );

    expect(screen.getByText(/mail accepted/i)).toBeTruthy();
    expect(screen.getByText(/active-session acknowledgement unavailable/i)).toBeTruthy();
    expect(screen.getByText(/no verified active-session acknowledgement event/i)).toBeTruthy();
    expect(screen.queryByText(/delivered/i)).toBeNull();
    expect(screen.queryByText(/session acknowledged/i)).toBeNull();
  });

  it('labels exact-message mail read without saying the session acknowledged it', () => {
    render(
      <FollowUpDeliveryStatus
        state={{
          mail: 'read',
          acknowledgement: 'unavailable',
          acknowledgementReason: 'verified_session_signal_missing',
          eventStream: 'available',
          messageId: 'mail-9',
        }}
      />,
    );

    expect(screen.getByText(/^Mail read$/i)).toBeTruthy();
    expect(screen.getByText(/active-session acknowledgement unavailable/i)).toBeTruthy();
    expect(screen.queryByText(/session acknowledged/i)).toBeNull();
  });

  it('reports when the exact mail-read observation timed out', () => {
    render(
      <FollowUpDeliveryStatus
        state={{
          mail: 'accepted',
          acknowledgement: 'unavailable',
          acknowledgementReason: 'verified_session_signal_missing',
          eventStream: 'timed_out',
          messageId: 'mail-9',
        }}
      />,
    );

    expect(screen.getByText(/no mail\.read event observed before timeout/i)).toBeTruthy();
    expect(screen.getByText(/active-session acknowledgement unavailable/i)).toBeTruthy();
  });
});
