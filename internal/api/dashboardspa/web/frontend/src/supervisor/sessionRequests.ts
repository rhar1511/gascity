import type { RequestReceipt } from 'gas-city-dashboard-shared/gc-supervisor';
import { activeCityOrThrow } from '../api/cityBase';
import { supervisorApi } from './client';

// Tracked follow-ups are tied to one session execution and its durable receipt.
// Provider submission and active-session acknowledgement remain separate states.
export function submitSessionRequest(
  sessionId: string,
  requestId: string,
  generation: number,
  message: string,
): Promise<RequestReceipt> {
  return supervisorApi().submitSessionRequest(activeCityOrThrow('submit session request'), sessionId, {
    request_id: requestId,
    generation,
    message,
  });
}

export function getSessionRequest(sessionId: string, requestId: string): Promise<RequestReceipt> {
  return supervisorApi().sessionRequest(
    activeCityOrThrow('read session request receipt'),
    sessionId,
    requestId,
  );
}
