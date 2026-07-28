const test = require("node:test");
const assert = require("node:assert/strict");

const {
    buildOptions,
    filterOptions,
    formatCurrentTimePreview,
    formatOffsetMinutes,
    normalizeSearch,
    optionFromValue,
    timezoneOffsetMinutes,
} = require("../../static/js/admin-timezone-picker.js");

test("timezone picker presents automatic, suggested, IANA, and fixed-offset choices", () => {
    const options = buildOptions(
        ["", "UTC", "Europe/Paris", "America/Toronto", "-05:00"],
        {
            suggested: ["America/Toronto", "UTC"],
            systemTimezone: "America/Toronto",
            now: new Date("2026-01-15T12:00:00Z"),
        },
    );

    assert.deepEqual(options.map(option => option.group), [
        "Automatic",
        "Suggested",
        "Suggested",
        "Timezones",
        "Fixed UTC offsets",
    ]);
    assert.equal(
        options.find(option => option.value === "").secondary,
        "Detected at startup: UTC\u221205:00 \u00b7 America/Toronto \u00b7 Follows server setting after restart",
    );
    assert.equal(options.some(option => option.value === "Local"), false);
    assert.equal(options.find(option => option.value === "America/Toronto").primary, "Toronto");
    assert.match(options.find(option => option.value === "Europe/Paris").secondary, /Europe\/Paris/);
    assert.equal(options.find(option => option.value === "-05:00").primary, "UTC\u221205:00");
});

test("timezone search accepts city, canonical region, and UTC offset terms", () => {
    const options = buildOptions(
        ["America/Toronto", "America/New_York", "Europe/Paris", "+05:30", "-05:30"],
        { now: new Date("2026-01-15T12:00:00Z") },
    );

    assert.equal(filterOptions(options, "toronto")[0].value, "America/Toronto");
    assert.deepEqual(
        filterOptions(options, "america new").map(option => option.value),
        ["America/New_York"],
    );
    assert.deepEqual(
        filterOptions(options, "utc +05:30").map(option => option.value),
        ["+05:30"],
    );
    assert.deepEqual(
        filterOptions(options, "utc -05:30").map(option => option.value),
        ["-05:30"],
    );
    assert.equal(filterOptions(options, "not-a-timezone").length, 0);
});

test("timezone labels expose current offsets without changing canonical values", () => {
    const utc = optionFromValue("UTC", { now: new Date("2026-07-27T12:00:00Z") });
    const toronto = optionFromValue("America/Toronto", { now: new Date("2026-07-27T12:00:00Z") });
    const fixed = optionFromValue("+05:45");

    assert.equal(utc.value, "UTC");
    assert.equal(utc.secondary, "Explicit timezone \u00b7 UTC+00:00 \u00b7 UTC");
    assert.equal(toronto.secondary, "Explicit timezone \u00b7 UTC\u221204:00 \u00b7 America/Toronto");
    assert.equal(fixed.value, "+05:45");
    assert.equal(fixed.secondary, "Fixed offset \u00b7 no daylight-saving changes");
    assert.equal(timezoneOffsetMinutes("+05:45"), 345);
    assert.equal(formatOffsetMinutes(-210), "UTC\u221203:30");
});

test("timezone search normalization handles IANA separators and accents", () => {
    assert.equal(normalizeSearch("  America/Argentina/Buenos_Aires "), "america argentina buenos aires");
    assert.equal(normalizeSearch("Montr\u00e9al"), "montreal");
});

test("current app time preview uses the accepted resolved timezone", () => {
    assert.equal(
        formatCurrentTimePreview("America/Toronto", new Date("2026-07-28T00:58:00Z")),
        "Current app time: 2026-07-27 20:58 EDT \u00b7 America/Toronto",
    );
    assert.equal(
        formatCurrentTimePreview("+05:30", new Date("2026-07-28T00:58:00Z")),
        "Current app time: 2026-07-28 06:28 UTC+05:30 \u00b7 +05:30",
    );
});
