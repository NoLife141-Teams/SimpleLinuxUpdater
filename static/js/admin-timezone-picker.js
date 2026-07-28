(function initAdminTimezonePicker(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.AdminTimezonePicker = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function adminTimezonePickerFactory() {
    "use strict";

    const automaticValues = new Set([""]);
    const fixedOffsetPattern = /^[+-]\d{2}:\d{2}$/;

    function normalizeSearch(value) {
        return String(value || "")
            .normalize("NFD")
            .replace(/[\u0300-\u036f]/g, "")
            .replace(/[_/]+/g, " ")
            .replace(/\s+/g, " ")
            .trim()
            .toLowerCase();
    }

    function cityName(value) {
        const parts = String(value || "").split("/");
        return (parts[parts.length - 1] || value).replace(/_/g, " ");
    }

    function formatOffsetMinutes(minutes) {
        const numeric = Number(minutes);
        if (!Number.isFinite(numeric) || numeric === 0) return "UTC+00:00";
        const sign = numeric < 0 ? "\u2212" : "+";
        const absolute = Math.abs(numeric);
        const hours = String(Math.floor(absolute / 60)).padStart(2, "0");
        const remainder = String(absolute % 60).padStart(2, "0");
        return `UTC${sign}${hours}:${remainder}`;
    }

    function fixedOffsetMinutes(value) {
        const match = String(value || "").match(/^([+-])(\d{2}):(\d{2})$/);
        if (!match) return null;
        const minutes = (Number(match[2]) * 60) + Number(match[3]);
        return match[1] === "-" ? -minutes : minutes;
    }

    function timezoneOffsetMinutes(timezone, now = new Date()) {
        if (timezone === "UTC") return 0;
        if (fixedOffsetPattern.test(timezone)) return fixedOffsetMinutes(timezone);
        if (!timezone || timezone === "Local" || typeof Intl === "undefined") return null;
        try {
            const formatter = new Intl.DateTimeFormat("en-CA", {
                timeZone: timezone,
                timeZoneName: "longOffset",
                hour: "2-digit"
            });
            const name = formatter.formatToParts(now).find(part => part.type === "timeZoneName")?.value || "";
            if (name === "GMT" || name === "UTC") return 0;
            const match = name.match(/(?:GMT|UTC)([+-])(\d{1,2})(?::?(\d{2}))?/);
            if (!match) return null;
            const minutes = (Number(match[2]) * 60) + Number(match[3] || 0);
            return match[1] === "-" ? -minutes : minutes;
        } catch (_) {
            return null;
        }
    }

    function formatCurrentTimePreview(timezone, now = new Date()) {
        const resolved = String(timezone || "").trim() || "UTC";
        try {
            const fixedMinutes = fixedOffsetPattern.test(resolved) ? fixedOffsetMinutes(resolved) : null;
            const displayDate = fixedMinutes === null
                ? now
                : new Date(now.getTime() + (fixedMinutes * 60 * 1000));
            const formatter = new Intl.DateTimeFormat("en-CA", {
                timeZone: fixedMinutes === null ? resolved : "UTC",
                year: "numeric",
                month: "2-digit",
                day: "2-digit",
                hour: "2-digit",
                minute: "2-digit",
                hourCycle: "h23",
                ...(fixedMinutes === null ? { timeZoneName: "short" } : {}),
            });
            const parts = Object.fromEntries(
                formatter.formatToParts(displayDate)
                    .filter(part => part.type !== "literal")
                    .map(part => [part.type, part.value]),
            );
            const zoneLabel = fixedMinutes === null ? (parts.timeZoneName || resolved) : formatOffsetMinutes(fixedMinutes);
            return `Current app time: ${parts.year}-${parts.month}-${parts.day} ${parts.hour}:${parts.minute} ${zoneLabel} \u00b7 ${resolved}`;
        } catch (_) {
            return `Current app time unavailable \u00b7 ${resolved}`;
        }
    }

    function optionFromValue(value, options = {}) {
        const rawTimezone = String(value || "").trim();
        const timezone = rawTimezone === "Local" ? "" : rawTimezone;
        if (timezone === "") {
            const systemTimezone = String(options.systemTimezone || "").trim();
            const detected = systemTimezone && systemTimezone !== "Local"
                ? optionFromValue(systemTimezone, { now: options.now, detected: true })
                : null;
            const detectedLabel = detected?.secondary || systemTimezone;
            return {
                value: "",
                primary: "System default timezone",
                secondary: detectedLabel
                    ? `Detected at startup: ${detectedLabel} \u00b7 Follows server setting after restart`
                    : "Uses the server timezone detected when saved",
                group: "Automatic",
                search: `system default automatic server timezone ${systemTimezone} ${detectedLabel}`
            };
        }
        if (fixedOffsetPattern.test(timezone)) {
            const offset = formatOffsetMinutes(fixedOffsetMinutes(timezone));
            return {
                value: timezone,
                primary: offset,
                secondary: "Fixed offset \u00b7 no daylight-saving changes",
                group: "Fixed UTC offsets",
                search: `${timezone} ${offset} fixed utc offset daylight saving`
            };
        }

        const offsetMinutes = timezoneOffsetMinutes(timezone, options.now);
        const offset = offsetMinutes === null ? "" : formatOffsetMinutes(offsetMinutes);
        const primary = timezone === "UTC" ? "Coordinated Universal Time" : cityName(timezone);
        const canonical = timezone.replace(/_/g, " ");
        return {
            value: timezone,
            primary,
            secondary: [options.detected ? "" : "Explicit timezone", offset, canonical].filter(Boolean).join(" \u00b7 "),
            group: options.suggested?.has(timezone) ? "Suggested" : "Timezones",
            search: `${primary} ${canonical} ${offset} ${timezone === "UTC" ? "gmt coordinated universal" : ""}`
        };
    }

    function buildOptions(values, options = {}) {
        const suggested = new Set(Array.isArray(options.suggested) ? options.suggested : []);
        const seen = new Set();
        return (Array.isArray(values) ? values : [])
            .map(value => String(value || "").trim())
            .map(value => value === "Local" ? "" : value)
            .filter(value => !seen.has(value) && seen.add(value))
            .map(value => optionFromValue(value, { ...options, suggested }))
            .sort((a, b) => {
                const groupOrder = { Automatic: 0, Suggested: 1, Timezones: 2, "Fixed UTC offsets": 3 };
                const groupDifference = groupOrder[a.group] - groupOrder[b.group];
                if (groupDifference !== 0) return groupDifference;
                if (automaticValues.has(a.value) || automaticValues.has(b.value)) return 0;
                if (a.group === "Fixed UTC offsets") {
                    return fixedOffsetMinutes(a.value) - fixedOffsetMinutes(b.value);
                }
                return a.primary.localeCompare(b.primary) || a.value.localeCompare(b.value);
            });
    }

    function matchScore(option, query) {
        const normalized = normalizeSearch(query);
        if (!normalized) return 1;
        const terms = normalized.split(" ");
        const primary = normalizeSearch(option.primary);
        const value = normalizeSearch(option.value);
        const search = normalizeSearch(option.search);
        if (!terms.every(term => search.includes(term))) return -1;
        if (primary === normalized || value === normalized) return 100;
        if (primary.startsWith(normalized)) return 80;
        if (value.startsWith(normalized)) return 70;
        if (primary.includes(normalized)) return 60;
        if (value.includes(normalized)) return 50;
        return 10;
    }

    function filterOptions(options, query) {
        const normalized = normalizeSearch(query);
        if (!normalized) return options.slice();
        return options
            .map(option => ({ option, score: matchScore(option, normalized) }))
            .filter(item => item.score >= 0)
            .sort((a, b) => b.score - a.score || a.option.primary.localeCompare(b.option.primary))
            .map(item => item.option);
    }

    function createPicker(config = {}) {
        const trigger = config.trigger;
        const popover = config.popover;
        const search = config.search;
        const listbox = config.listbox;
        const empty = config.empty;
        if (!trigger || !popover || !search || !listbox || !empty) return null;

        let options = Array.isArray(config.options) ? config.options.slice() : [];
        let selectedValue = "";
        let activeIndex = -1;
        let rendered = [];
        let open = false;

        const triggerLabel = trigger.querySelector(".timezone-picker-trigger-label");
        const triggerDetail = trigger.querySelector(".timezone-picker-trigger-detail");

        function selectedOption() {
            return options.find(option => option.value === selectedValue) || optionFromValue(selectedValue);
        }

        function updateTrigger() {
            const option = selectedOption();
            trigger.value = option.value;
            if (triggerLabel) triggerLabel.textContent = option.primary;
            if (triggerDetail) triggerDetail.textContent = option.secondary;
        }

        function optionID(index) {
            return `app-timezone-option-${index}`;
        }

        function setActive(index, shouldScroll = false) {
            if (rendered.length === 0) {
                activeIndex = -1;
                search.removeAttribute("aria-activedescendant");
                return;
            }
            activeIndex = Math.max(0, Math.min(index, rendered.length - 1));
            const optionNodes = listbox.querySelectorAll('[role="option"]');
            optionNodes.forEach((node, nodeIndex) => node.classList.toggle("is-active", nodeIndex === activeIndex));
            const activeNode = optionNodes[activeIndex];
            if (!activeNode) return;
            search.setAttribute("aria-activedescendant", activeNode.id);
            if (shouldScroll) activeNode.scrollIntoView({ block: "nearest" });
        }

        function render(query = "") {
            rendered = filterOptions(options, query);
            listbox.replaceChildren();
            empty.hidden = rendered.length !== 0;
            listbox.hidden = rendered.length === 0;
            let previousGroup = "";

            rendered.forEach((option, index) => {
                const displayedGroup = String(query || "").trim() ? "Search results" : option.group;
                if (displayedGroup !== previousGroup) {
                    const heading = document.createElement("div");
                    heading.className = "timezone-picker-group";
                    heading.textContent = displayedGroup;
                    heading.setAttribute("aria-hidden", "true");
                    listbox.appendChild(heading);
                    previousGroup = displayedGroup;
                }

                const row = document.createElement("button");
                row.className = "timezone-picker-option";
                row.type = "button";
                row.id = optionID(index);
                row.setAttribute("role", "option");
                row.setAttribute("aria-selected", String(option.value === selectedValue));
                row.dataset.index = String(index);
                row.dataset.value = option.value;

                const copy = document.createElement("span");
                copy.className = "timezone-picker-option-copy";
                const primary = document.createElement("span");
                primary.className = "timezone-picker-option-label";
                primary.textContent = option.primary;
                const secondary = document.createElement("span");
                secondary.className = "timezone-picker-option-detail";
                secondary.textContent = option.secondary;
                copy.append(primary, secondary);

                const check = document.createElement("span");
                check.className = "timezone-picker-option-check";
                check.setAttribute("aria-hidden", "true");
                row.append(copy, check);
                listbox.appendChild(row);
            });

            const selectedIndex = rendered.findIndex(option => option.value === selectedValue);
            setActive(selectedIndex >= 0 ? selectedIndex : 0);
        }

        function setOpen(nextOpen) {
            open = Boolean(nextOpen);
            popover.hidden = !open;
            trigger.setAttribute("aria-expanded", String(open));
            search.setAttribute("aria-expanded", String(open));
            if (!open) {
                search.value = "";
                search.removeAttribute("aria-activedescendant");
                return;
            }
            render("");
            window.requestAnimationFrame(() => search.focus());
        }

        function choose(option) {
            if (!option) return;
            selectedValue = option.value;
            updateTrigger();
            setOpen(false);
            trigger.focus();
            trigger.dispatchEvent(new Event("input", { bubbles: true }));
            if (typeof config.onChange === "function") config.onChange(selectedValue);
        }

        function setValue(value) {
            const normalized = String(value || "").trim();
            if (!options.some(option => option.value === normalized) && normalized) {
                options.push(optionFromValue(normalized));
            }
            selectedValue = normalized;
            updateTrigger();
            if (open) render(search.value);
        }

        trigger.addEventListener("click", () => setOpen(!open));
        trigger.addEventListener("keydown", event => {
            if (event.key === "ArrowDown" || event.key === "ArrowUp") {
                event.preventDefault();
                setOpen(true);
            }
        });
        search.addEventListener("input", () => render(search.value));
        search.addEventListener("keydown", event => {
            if (event.key === "ArrowDown") {
                event.preventDefault();
                setActive(activeIndex + 1, true);
            } else if (event.key === "ArrowUp") {
                event.preventDefault();
                setActive(activeIndex - 1, true);
            } else if (event.key === "Enter") {
                event.preventDefault();
                choose(rendered[activeIndex]);
            } else if (event.key === "Escape") {
                event.preventDefault();
                setOpen(false);
                trigger.focus();
            } else if (event.key === "Tab") {
                setOpen(false);
            }
        });
        listbox.addEventListener("mousemove", event => {
            const row = event.target.closest('[role="option"]');
            if (row) setActive(Number(row.dataset.index));
        });
        listbox.addEventListener("click", event => {
            const row = event.target.closest('[role="option"]');
            if (row) choose(rendered[Number(row.dataset.index)]);
        });
        document.addEventListener("pointerdown", event => {
            if (open && !popover.contains(event.target) && !trigger.contains(event.target)) setOpen(false);
        });

        updateTrigger();
        return Object.freeze({
            getValue: () => selectedValue,
            setValue,
            setOptions(nextOptions) {
                options = Array.isArray(nextOptions) ? nextOptions.slice() : [];
                updateTrigger();
                if (open) render(search.value);
            },
            setSystemTimezone(systemTimezone) {
                const index = options.findIndex(option => option.value === "");
                if (index >= 0) options[index] = optionFromValue("", { systemTimezone });
                updateTrigger();
                if (open) render(search.value);
            },
            open: () => setOpen(true),
            close: () => setOpen(false)
        });
    }

    return Object.freeze({
        buildOptions,
        cityName,
        createPicker,
        filterOptions,
        formatCurrentTimePreview,
        formatOffsetMinutes,
        normalizeSearch,
        optionFromValue,
        timezoneOffsetMinutes
    });
}));
