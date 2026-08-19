export const MACHINE_NICKNAME_MAX = 64;

export function normalizeMachineNickname(value: unknown): { nickname: string; error?: string } {
  const nickname = String(value ?? '').trim();
  if (Array.from(nickname).length > MACHINE_NICKNAME_MAX) {
    return { nickname, error: `El apodo puede tener hasta ${MACHINE_NICKNAME_MAX} caracteres` };
  }
  if (/\p{Cc}/u.test(nickname)) {
    return { nickname, error: 'El apodo no puede contener caracteres de control' };
  }
  return { nickname };
}
