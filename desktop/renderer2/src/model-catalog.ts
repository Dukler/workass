export type WorkassRuntimeProfile = 'prod' | 'dev' | 'test';

export function workassRuntimeProfile(value: unknown): WorkassRuntimeProfile {
  return value === 'dev' || value === 'test' ? value : 'prod';
}
