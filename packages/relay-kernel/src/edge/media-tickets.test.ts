import { describe, expect, it } from "vitest";

import {
  isIssuePath,
  MEDIA_TICKET_IDLE_MS,
  MEDIA_TICKET_MAX_MS,
  MediaTickets,
  mediaTargetPath,
  redeemTicketOf,
} from "./media-tickets.ts";

const GRANT = {
  sourceId: "0123456789abcdef0123456789abcdef",
  path: "/api/media/abc",
  subject: "acct_test",
  jti: "j1",
};

describe("媒体票", () => {
  it("闲置期内可多次用并续期，闲置或签出过久作废", () => {
    let now = 0;
    const tickets = new MediaTickets(() => now);
    const { ticket, expiresAtMs } = tickets.issue(GRANT);
    expect(ticket).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(expiresAtMs).toBe(MEDIA_TICKET_IDLE_MS);
    now += MEDIA_TICKET_IDLE_MS - 1;
    expect(tickets.use(ticket)?.path).toBe("/api/media/abc");
    now += MEDIA_TICKET_IDLE_MS;
    expect(tickets.use(ticket)).toBeUndefined();

    const again = tickets.issue(GRANT).ticket;
    for (let at = 0; at < MEDIA_TICKET_MAX_MS; at += MEDIA_TICKET_IDLE_MS / 2) {
      now += MEDIA_TICKET_IDLE_MS / 2;
      tickets.use(again);
    }
    expect(tickets.use(again)).toBeUndefined();
  });

  it("兑票路径只认一段 43 字符的票；签票路径不看 query", () => {
    const ticket = "A".repeat(43);
    expect(redeemTicketOf(`/_relay/m/${ticket}`)).toBe(ticket);
    expect(redeemTicketOf(`/_relay/m/${ticket}?x=1`)).toBeNull();
    expect(redeemTicketOf(`/_relay/m/${ticket}/x`)).toBeNull();
    expect(redeemTicketOf("/_relay/m/short")).toBeNull();
    expect(isIssuePath("/_relay/media-tickets")).toBe(true);
    expect(isIssuePath("/_relay/media-tickets?x")).toBe(true);
    expect(isIssuePath("/_relay/media-ticketsx")).toBe(false);
  });

  it("要转的路径：以 / 开头、不碰 /_relay/、不带 .. 与控制字符", () => {
    expect(mediaTargetPath({ path: "/api/media/abc" })).toBe("/api/media/abc");
    expect(mediaTargetPath({ path: "/api/x?y=1" })).toBe("/api/x?y=1");
    for (const path of [
      "api/x",
      "//evil.example/x",
      "/_relay/m/x",
      "/api/../x",
      "/api/a b",
      "/api/x#f",
      "/api\\x",
      "/".padEnd(3000, "a"),
      42,
    ])
      expect(mediaTargetPath({ path })).toBeNull();
    expect(mediaTargetPath(null)).toBeNull();
    expect(mediaTargetPath("x")).toBeNull();
  });
});
