/**
 * 账号标识（`sub`）：不透明字符串。personal 的账号本人是 `acct:<账号名>`，经链接进来的访客是
 * `guest:<linkId>:<随机>`。
 */

/** `acct:<账号名>`：personal 唯一账号的 accountId。 */
export function ownerSubject(name: string): string {
  return `acct:${name}`;
}

export function guestPrefix(linkId: string): string {
  return `guest:${linkId}:`;
}

/** `guest:<linkId>:<随机>` → linkId；不是访客答 null。 */
export function guestLinkId(subject: string): string | null {
  const match = /^guest:([0-9a-f]{16}):[0-9a-f]+$/.exec(subject);
  return match?.[1] ?? null;
}
