export interface WorkassQuestionAnswerInput {
  status: 'answered' | 'dismissed';
  selectedOptionIds?: string[];
  freeText?: string;
}

export const WORKASS_QUESTION_TEXT_LIMIT = 1000;

// JavaScript length and HTML maxlength use UTF-16 units, while the shared
// question contract caps Unicode code points. Keep the UI's input bound in the
// same unit as daemon validation so astral text can use the full limit.
export function limitWorkassQuestionText(value: string): string {
  return Array.from(value).slice(0, WORKASS_QUESTION_TEXT_LIMIT).join('');
}

// The existing chat:permission-decide wire accepts one optionId string. This
// tagged, base64url JSON token carries the structured Workass answer through
// that unchanged field; the daemon validates it against the owning question.
export function encodeWorkassQuestionAnswer(answer: WorkassQuestionAnswerInput): string {
  const bytes = new TextEncoder().encode(JSON.stringify(answer));
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  const base64url = btoa(binary).replace(/=/g, '').replace(/\+/g, '-').replace(/\//g, '_');
  return `workass-question-v1:${base64url}`;
}
