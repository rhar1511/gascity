import type { FollowUpDeliveryState } from './followUpDelivery';

interface FollowUpDeliveryStatusProps {
  state: FollowUpDeliveryState;
}

const mailLabels: Record<FollowUpDeliveryState['mail'], string> = {
  not_submitted: 'Preparing follow-up',
  submitted: 'Follow-up submitted',
  accepted: 'Mail accepted',
  read: 'Mail read',
  failed: 'Mail not accepted',
};

export function FollowUpDeliveryStatus({
  state,
}: FollowUpDeliveryStatusProps) {
  return (
    <p
      role="status"
      aria-live="polite"
      data-mail-status={state.mail}
      data-acknowledgement-status={state.acknowledgement}
      className="text-xs text-slate-400"
    >
      <span>{mailLabels[state.mail]}</span>
      <span aria-hidden="true"> · </span>
      <span>Active-session acknowledgement unavailable</span>
      <span> · Gas City has no verified active-session acknowledgement event</span>
      {state.eventStream === 'unavailable' && <span> · Mail-read event stream unavailable</span>}
      {state.eventStream === 'timed_out' && (
        <span> · No mail.read event observed before timeout</span>
      )}
    </p>
  );
}
