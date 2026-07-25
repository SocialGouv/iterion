import { describe, expect, it } from "vitest";

import { humanizeCron } from "./humanizeCron";

describe("humanizeCron", () => {
  it("renders the minute-cadence forms", () => {
    expect(humanizeCron("* * * * *")).toBe("every minute");
    // */1 is spelled out as the singular form, not "every 1 minutes".
    expect(humanizeCron("*/1 * * * *")).toBe("every minute");
    expect(humanizeCron("*/5 * * * *")).toBe("every 5 minutes");
    expect(humanizeCron("*/30 * * * *")).toBe("every 30 minutes");
  });

  it("renders the hourly fixed-minute form", () => {
    expect(humanizeCron("0 * * * *")).toBe("hourly at :00");
    expect(humanizeCron("15 * * * *")).toBe("hourly at :15");
  });

  it("renders the every-N-hours form", () => {
    // M */1 is the hourly cadence, not "every 1 hours".
    expect(humanizeCron("0 */1 * * *")).toBe("hourly at :00");
    expect(humanizeCron("30 */6 * * *")).toBe("every 6 hours at :30");
  });

  it("renders the daily form with zero-padded times", () => {
    expect(humanizeCron("0 3 * * *")).toBe("daily at 03:00");
    expect(humanizeCron("5 0 * * *")).toBe("daily at 00:05");
    expect(humanizeCron("30 17 * * *")).toBe("daily at 17:30");
  });

  it("renders a weekly single day (numeric)", () => {
    // The sec-audit schedule shape from CLAUDE.md: Monday 02:00 UTC.
    expect(humanizeCron("0 2 * * 1")).toBe("every Monday at 02:00");
    expect(humanizeCron("0 9 * * 5")).toBe("every Friday at 09:00");
  });

  it("accepts 3-letter day names, case-insensitively", () => {
    expect(humanizeCron("0 3 * * mon")).toBe("every Monday at 03:00");
    expect(humanizeCron("0 3 * * FRI")).toBe("every Friday at 03:00");
  });

  it("maps both 0 and 7 to Sunday", () => {
    expect(humanizeCron("0 8 * * 0")).toBe("every Sunday at 08:00");
    expect(humanizeCron("0 8 * * 7")).toBe("every Sunday at 08:00");
  });

  it("renders a weekly day range", () => {
    expect(humanizeCron("0 7 * * 1-5")).toBe("every Monday to Friday at 07:00");
    expect(humanizeCron("30 18 * * mon-fri")).toBe("every Monday to Friday at 18:30");
    expect(humanizeCron("0 9 * * 6-7")).toBe("every Saturday to Sunday at 09:00");
  });

  it("returns null for a malformed day range", () => {
    expect(humanizeCron("0 7 * * 1-funday")).toBeNull();
    expect(humanizeCron("0 7 * * 1-5-6")).toBeNull();
  });

  it("renders a weekly comma list of days", () => {
    expect(humanizeCron("0 9 * * 1,3,5")).toBe(
      "every Monday, Wednesday, Friday at 09:00",
    );
    expect(humanizeCron("30 18 * * sat,sun")).toBe(
      "every Saturday, Sunday at 18:30",
    );
  });

  it("renders the monthly fixed-day form", () => {
    expect(humanizeCron("0 4 1 * *")).toBe("monthly on day 1 at 04:00");
    expect(humanizeCron("15 12 28 * *")).toBe("monthly on day 28 at 12:15");
  });

  // The conservative contract: anything outside the recognised shapes
  // returns null so the caller shows only the raw expression — a missing
  // hint beats a wrong translation.
  it.each([
    ["", "empty string"],
    ["0 3 * *", "4 fields"],
    ["0 3 * * * *", "6 fields (seconds form)"],
    ["0 3 * 6 *", "month-restricted"],
    ["0 3 * jan *", "month name"],
    ["0-30 3 * * *", "minute range"],
    ["0 9-17 * * *", "hour range"],
    ["0,30 3 * * *", "minute comma list"],
    ["0 3 1 * 1", "both day-of-month and day-of-week set"],
    ["0 3 32 * *", "day-of-month out of range"],
    ["0 3 0 * *", "day-of-month zero"],
    ["0 3 * * 8", "numeric day-of-week out of range"],
    ["0 3 * * monday", "full day name (only 3-letter accepted)"],
    ["0 3 * * 1,funday", "comma list with an unknown day"],
    ["x 3 * * *", "non-numeric minute"],
    ["0 x * * *", "non-numeric hour"],
    ["*/x * * * *", "non-numeric step"],
  ])("returns null for %s (%s)", (expr) => {
    expect(humanizeCron(expr)).toBeNull();
  });
});

describe("humanizeCron with CRON_TZ prefix", () => {
  it("humanizes the schedule part and appends the zone", () => {
    expect(humanizeCron("CRON_TZ=Europe/Paris 0 8 * * *")).toBe(
      "daily at 08:00 (Europe/Paris)",
    );
    expect(humanizeCron("CRON_TZ=Europe/Paris 0 8 * * 1")).toBe(
      "every Monday at 08:00 (Europe/Paris)",
    );
  });

  it("returns null when the schedule part is not humanizable", () => {
    expect(humanizeCron("CRON_TZ=Europe/Paris 0 8 1,15 * *")).toBeNull();
  });
});
