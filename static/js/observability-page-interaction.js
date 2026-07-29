(function initObservabilityPageInteraction(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.ObservabilityPageInteraction = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function observabilityPageInteractionFactory() {
    "use strict";

    const validWindows = new Set(["24h", "7d", "30d"]);
    const validAttentionFilters = new Set(["all", "failures", "apt", "disk", "reboot", "stale", "missing"]);
    const validSorts = new Set(["attention", "name", "freshness", "packages", "security", "disk", "failures"]);

    function clone(value) {
        if (Array.isArray(value)) return value.map(clone);
        if (!value || typeof value !== "object") return value;
        return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, clone(item)]));
    }

    function createSource() {
        return { status: "unavailable", data: null, error: null, requestID: null, generation: 0 };
    }

    function projectFleetHealth(input = {}) {
        const updatesTotal = Number(input.updatesTotal || 0);
        const successRate = Number(input.successRate || 0);
        if (updatesTotal <= 0) return { state: "neutral", label: "No data" };
        if (successRate >= 95) return { state: "healthy", label: "Healthy" };
        if (successRate >= 80) return { state: "degraded", label: "Degraded" };
        return { state: "critical", label: "Critical" };
    }

    function projectDurationConfidence(input = {}) {
        const samples = Math.max(0, Number(input.samples || 0));
        const total = Math.max(0, Number(input.total || 0));
        if (samples === 0 || total === 0) return { state: "no-data", label: "No duration data" };
        if (samples / total < 0.5) return { state: "low", label: "Low confidence" };
        return { state: "representative", label: "Representative" };
    }

    function projectSourceLifecycle(source, state = {}) {
        const hasData = Boolean(state.data);
        const acceptedAt = state.data?.generated_at || state.data?.to || "";
        if (state.status === "refreshing") {
            return {
                state: hasData ? "refreshing" : "loading",
                label: hasData ? "Refreshing" : "Loading",
                acceptedAt,
                detail: hasData ? "Last accepted data retained" : `Waiting for ${source === "summary" ? "update metrics" : "host health"}`,
                retry: false,
            };
        }
        if (state.status === "fresh") {
            return { state: "current", label: "Current", acceptedAt, detail: "Accepted data", retry: false };
        }
        if (state.status === "stale") {
            return { state: "stale", label: "Stale", acceptedAt, detail: "Showing last accepted data", retry: true };
        }
        return { state: "unavailable", label: "Unavailable", acceptedAt: "", detail: "No accepted data", retry: true };
    }

    function validDiskKB(value) {
        const disk = Number(value);
        return Number.isFinite(disk) && disk > 0 ? disk : null;
    }

    function observationIsStale(server, windowValue, nowMS) {
        const observation = server?.latest || server?.last_observation;
        if (!observation) return false;
        const captured = Date.parse(observation.captured_at || "");
        if (!Number.isFinite(captured)) return true;
        const maximumAge = windowValue === "24h" ? 24 * 60 * 60 * 1000 : 48 * 60 * 60 * 1000;
        return nowMS - captured > maximumAge;
    }

    function healthAttentionScore(server, windowValue, nowMS) {
        let score = 0;
        score += (Number(server.update_failures || 0) + Number(server.scan_failures || 0)) * 100;
        score += (Number(server.apt_problem_samples || 0) + Number(server.disk_problem_samples || 0)) * 20;
        if (server.reboot_seen) score += 10;
        if (observationIsStale(server, windowValue, nowMS)) score += 5;
        const observation = server?.latest || server?.last_observation;
        if (!validDiskKB(observation?.disk_free_kb)) score += 2;
        return score;
    }

    function matchesAttention(server, filter, windowValue, nowMS) {
        if (filter === "failures") return Number(server.update_failures || 0) + Number(server.scan_failures || 0) > 0;
        if (filter === "apt") return Number(server.apt_problem_samples || 0) > 0;
        if (filter === "disk") return Number(server.disk_problem_samples || 0) > 0;
        if (filter === "reboot") return Boolean(server.reboot_seen);
        if (filter === "stale") return observationIsStale(server, windowValue, nowMS);
        if (filter === "missing") {
            const observation = server.latest || server.last_observation;
            return !observation || !validDiskKB(observation.disk_free_kb);
        }
        return true;
    }

    function projectHealthCollection(servers, options = {}) {
        const search = String(options.search || "").trim().toLowerCase();
        const attention = validAttentionFilters.has(options.attention) ? options.attention : "all";
        const sort = validSorts.has(options.sort) ? options.sort : "attention";
        const windowValue = validWindows.has(options.window) ? options.window : "7d";
        const nowMS = Number(options.nowMS) || Date.now();
        const items = (Array.isArray(servers) ? servers : []).filter(server =>
            (!search || String(server.name || "").toLowerCase().includes(search))
            && matchesAttention(server, attention, windowValue, nowMS)
        );
        items.sort((left, right) => {
            const leftObservation = left?.latest || left?.last_observation;
            const rightObservation = right?.latest || right?.last_observation;
            if (sort === "name") return String(left.name || "").localeCompare(String(right.name || ""));
            if (sort === "freshness") return Date.parse(rightObservation?.captured_at || 0) - Date.parse(leftObservation?.captured_at || 0);
            if (sort === "packages") return Number(right.latest?.package_count || 0) - Number(left.latest?.package_count || 0);
            if (sort === "security") return Number(right.latest?.security_count || 0) - Number(left.latest?.security_count || 0);
            if (sort === "disk") return Number(rightObservation?.disk_free_kb || 0) - Number(leftObservation?.disk_free_kb || 0);
            if (sort === "failures") {
                return (Number(right.update_failures || 0) + Number(right.scan_failures || 0))
                    - (Number(left.update_failures || 0) + Number(left.scan_failures || 0));
            }
            return healthAttentionScore(right, windowValue, nowMS) - healthAttentionScore(left, windowValue, nowMS)
                || String(left.name || "").localeCompare(String(right.name || ""));
        });
        const pageSize = 25;
        const pageCount = Math.max(1, Math.ceil(items.length / pageSize));
        const page = Math.min(Math.max(1, Number.parseInt(options.page, 10) || 1), pageCount);
        return {
            items,
            visibleItems: items.slice((page - 1) * pageSize, page * pageSize),
            total: items.length,
            page,
            pageCount,
            pageSize,
            staleNames: items.filter(server => observationIsStale(server, windowValue, nowMS)).map(server => server.name),
        };
    }

    function trendMetricValue(point, metric) {
        if (metric === "packages") return Number(point.package_count);
        if (metric === "security") return Number(point.security_count);
        if (metric === "disk") return validDiskKB(point.disk_free_kb);
        if (metric === "failures") {
            const failed = ["failure", "failed", "error", "cancelled", "interrupted"];
            const updateFailed = failed.includes(String(point.last_update_status || "").toLowerCase()) ? 1 : 0;
            const scanFailed = failed.includes(String(point.last_scan_status || "").toLowerCase()) ? 1 : 0;
            return updateFailed + scanFailed;
        }
        return null;
    }

    function trendBucketUnit(windowValue) {
        if (windowValue === "24h") return "hour";
        return "day";
    }

    function createTrendBucketKeyer(windowValue, timeZone) {
        const bucketUnit = trendBucketUnit(windowValue);
        const formatterOptions = {
            timeZone: String(timeZone || "UTC"),
            year: "numeric",
            month: "2-digit",
            day: "2-digit",
            hour: "2-digit",
            hourCycle: "h23",
            timeZoneName: "longOffset",
        };
        let formatter;
        try {
            formatter = new Intl.DateTimeFormat("en-CA", formatterOptions);
        } catch (_) {
            formatter = new Intl.DateTimeFormat("en-CA", { ...formatterOptions, timeZone: "UTC" });
        }
        const localParts = timestamp => Object.fromEntries(
            formatter.formatToParts(new Date(timestamp))
                .filter(part => part.type !== "literal")
                .map(part => [part.type, part.value])
        );
        const offsetMinutes = raw => {
            const match = String(raw || "").match(/^GMT([+-])(\d{1,2})(?::?(\d{2}))?$/);
            if (!match) return 0;
            const minutes = Number(match[2]) * 60 + Number(match[3] || 0);
            return match[1] === "-" ? -minutes : minutes;
        };
        const canonicalStart = (bucketKey, unit, offsetName) => {
            const [year, month, day] = bucketKey.slice(0, 10).split("-").map(Number);
            const hour = unit === "hour" ? Number(bucketKey.slice(11, 13)) : 0;
            const targetAsUTC = Date.UTC(year, month - 1, day, hour);
            if (unit === "hour") {
                return new Date(targetAsUTC - offsetMinutes(offsetName) * 60_000).toISOString();
            }
            let candidate = targetAsUTC;
            for (let attempt = 0; attempt < 4; attempt += 1) {
                const projected = localParts(candidate);
                const projectedAsUTC = Date.UTC(
                    Number(projected.year),
                    Number(projected.month) - 1,
                    Number(projected.day),
                    Number(projected.hour)
                );
                const correction = targetAsUTC - projectedAsUTC;
                candidate += correction;
                if (correction === 0) break;
            }
            return new Date(candidate).toISOString();
        };
        return timestamp => {
            const parts = localParts(timestamp);
            const dayKey = `${parts.year}-${parts.month}-${parts.day}`;
            if (bucketUnit === "hour") {
                const bucketKey = `${dayKey}T${parts.hour}`;
                const offsetName = parts.timeZoneName || "GMT";
                return {
                    bucketID: `${bucketKey}|${offsetName}`,
                    bucketKey,
                    bucketUnit,
                    timestamp: canonicalStart(bucketKey, bucketUnit, offsetName),
                };
            }
            return {
                bucketID: dayKey,
                bucketKey: dayKey,
                bucketUnit,
                timestamp: canonicalStart(dayKey, bucketUnit, parts.timeZoneName),
            };
        };
    }

    function projectFleetTrendSeries(servers, metric, options = {}) {
        const events = [];
        (Array.isArray(servers) ? servers : []).forEach(server => {
            (Array.isArray(server.points) ? server.points : []).forEach(point => {
                const timestamp = String(point.captured_at || "");
                if (Number.isFinite(Date.parse(timestamp))) events.push({ server: String(server.name || ""), timestamp, point });
            });
        });
        events.sort((left, right) => Date.parse(left.timestamp) - Date.parse(right.timestamp) || left.server.localeCompare(right.server));
        const bucketFor = createTrendBucketKeyer(options.window || "7d", options.timeZone || "UTC");
        const buckets = new Map();
        events.forEach(event => {
            const bucket = bucketFor(event.timestamp);
            if (!buckets.has(bucket.bucketID)) {
                buckets.set(bucket.bucketID, { ...bucket, events: [] });
            }
            buckets.get(bucket.bucketID).events.push(event);
        });
        if (metric === "failures") {
            return [...buckets.values()].map(bucket => ({
                timestamp: bucket.timestamp,
                lastObservedAt: bucket.events[bucket.events.length - 1].timestamp,
                value: bucket.events.reduce((total, event) => total + trendMetricValue(event.point, metric), 0),
                samples: new Set(bucket.events.map(event => event.server)).size,
                observations: bucket.events.length,
                bucketKey: bucket.bucketKey,
                bucketUnit: bucket.bucketUnit,
            }));
        }
        const series = [];
        buckets.forEach(bucket => {
            const latestByServer = new Map();
            bucket.events.forEach(event => latestByServer.set(event.server, event.point));
            let total = 0;
            let samples = 0;
            latestByServer.forEach(point => {
                const value = trendMetricValue(point, metric);
                if (value === null || !Number.isFinite(value)) return;
                total += value;
                samples += 1;
            });
            if (samples === 0) {
                if (metric === "disk") {
                    series.push({
                        timestamp: bucket.timestamp,
                        lastObservedAt: bucket.events[bucket.events.length - 1].timestamp,
                        value: null,
                        samples: 0,
                        observations: bucket.events.length,
                        bucketKey: bucket.bucketKey,
                        bucketUnit: bucket.bucketUnit,
                    });
                }
                return;
            }
            series.push({
                timestamp: bucket.timestamp,
                lastObservedAt: bucket.events[bucket.events.length - 1].timestamp,
                value: total,
                samples,
                observations: bucket.events.length,
                bucketKey: bucket.bucketKey,
                bucketUnit: bucket.bucketUnit,
            });
        });
        return series;
    }

    function projectSelectedHostTrendSeries(servers, selectedHost, metric, options = {}) {
        const server = (Array.isArray(servers) ? servers : [])
            .find(item => String(item?.name || "") === selectedHost);
        if (!server) return [];
        const bucketFor = createTrendBucketKeyer(options.window || "7d", options.timeZone || "UTC");
        return (Array.isArray(server.points) ? server.points : [])
            .filter(point => Number.isFinite(Date.parse(point?.captured_at || "")))
            .map(point => {
                const timestamp = String(point.captured_at);
                const bucket = bucketFor(timestamp);
                return {
                    timestamp,
                    lastObservedAt: timestamp,
                    value: trendMetricValue(point, metric),
                    samples: 1,
                    observations: 1,
                    bucketKey: bucket.bucketKey,
                    bucketUnit: bucket.bucketUnit,
                };
            })
            .sort((left, right) => Date.parse(left.timestamp) - Date.parse(right.timestamp));
    }

    function niceChartMaximum(value, integer) {
        if (!Number.isFinite(value) || value <= 0) return 1;
        const magnitude = 10 ** Math.floor(Math.log10(value));
        const normalized = value / magnitude;
        const factors = integer ? [1, 2, 5, 10] : [1, 1.25, 1.5, 2, 2.5, 5, 10];
        const factor = factors.find(candidate => candidate >= normalized) || 10;
        return factor * magnitude;
    }

    function formatStorageKB(value) {
        const kb = Number(value || 0);
        if (!Number.isFinite(kb) || kb <= 0) return "-";
        const tb = kb / (1024 * 1024 * 1024);
        if (tb >= 1) return `${tb.toFixed(1)} TB`;
        const gb = kb / (1024 * 1024);
        if (gb >= 1) return `${gb.toFixed(1)} GB`;
        const mb = kb / 1024;
        if (mb >= 1) return `${mb.toFixed(0)} MB`;
        return `${kb.toFixed(0)} KB`;
    }

    function projectHealthTrendCSV(servers) {
        const header = [
            "Host",
            "Captured at app time",
            "Captured at UTC",
            "Source",
            "Packages",
            "Security updates",
            "Disk free KB",
            "Disk total KB",
            "APT status",
            "Disk status",
            "Update status",
            "Scan status",
            "Reboot required",
        ];
        const rows = (Array.isArray(servers) ? servers : []).flatMap(server => {
            const hasWindowObservation = Boolean(
                (Array.isArray(server?.points) && server.points.length) || server?.latest
            );
            const points = Array.isArray(server?.points) && server.points.length
                ? server.points
                : [server?.latest || server?.last_observation || null];
            return points.map(point => [
                String(server?.name || ""),
                point?.captured_at_display || point?.captured_at || "",
                point?.captured_at || "",
                point?.source || "",
                hasWindowObservation ? (point?.package_count ?? "") : "",
                hasWindowObservation ? (point?.security_count ?? "") : "",
                validDiskKB(point?.disk_free_kb) ?? "",
                validDiskKB(point?.disk_total_kb) ?? "",
                point?.apt_status || "",
                point?.disk_status || "",
                point?.last_update_status || "",
                point?.last_scan_status || "",
                point?.reboot_required == null ? "" : (point.reboot_required ? "yes" : "no"),
            ]);
        });
        return { header, rows };
    }

    function projectTrendChart(series, options = {}) {
        const timeline = (Array.isArray(series) ? series : [])
            .filter(point => Number.isFinite(Date.parse(point?.timestamp || "")))
            .map(point => {
                const rawValue = point?.value;
                const value = rawValue === null || rawValue === undefined || rawValue === ""
                    ? null
                    : Number(rawValue);
                return {
                    ...clone(point),
                    value: Number.isFinite(value) ? value : null,
                    timeMS: Date.parse(point.timestamp),
                };
            })
            .sort((left, right) => left.timeMS - right.timeMS);
        const valuedPoints = timeline.filter(point => point.value !== null);
        if (valuedPoints.length === 0) {
            return { points: [], segments: [], xTicks: [], yTicks: [], yMin: 0, yMax: 1 };
        }
        const yMax = Math.max(
            niceChartMaximum(Math.max(...valuedPoints.map(point => point.value)), Boolean(options.integer)),
            Number(options.minimumMax) || 0
        );
        const middle = options.integer ? Math.round(yMax / 2) : yMax / 2;
        const yTicks = [...new Set([0, middle, yMax])].sort((left, right) => left - right);
        const xMin = timeline[0].timeMS;
        const xMax = timeline[timeline.length - 1].timeMS;
        const xSpan = xMax - xMin;
        const xTickTimes = xSpan > 0 ? [xMin, xMin + xSpan / 2, xMax] : [xMin];
        const segments = [];
        let segment = [];
        timeline.forEach(({ timeMS, ...point }) => {
            if (point.value === null) {
                if (segment.length) segments.push(segment);
                segment = [];
                return;
            }
            segment.push({
                ...point,
                xRatio: xSpan > 0 ? (timeMS - xMin) / xSpan : 0.5,
                yRatio: point.value / yMax,
            });
        });
        if (segment.length) segments.push(segment);
        return {
            points: segments.flat(),
            segments,
            xTicks: xTickTimes.map(timeMS => new Date(timeMS).toISOString()),
            yTicks,
            yMin: 0,
            yMax,
        };
    }

    function createStore(options = {}) {
        const refreshDelayMs = Number(options.refreshDelayMs) > 0 ? Number(options.refreshDelayMs) : 15000;
        const now = typeof options.now === "function" ? options.now : Date.now;
        let selectedWindow = validWindows.has(options.window) ? options.window : "7d";
        let selectedHost = String(options.host || "").trim();
        let timeZone = String(options.timeZone || "UTC").trim() || "UTC";
        let search = String(options.search || "").trim();
        let attention = validAttentionFilters.has(options.attention) ? options.attention : "all";
        let sort = validSorts.has(options.sort) ? options.sort : "attention";
        let page = Math.max(1, Number.parseInt(options.page, 10) || 1);
        let knownHosts = [];
        let pageVisible = false;
        let generation = 0;
        let nextRequestID = 1;
        let fullGeneration = null;
        const sources = { summary: createSource(), trends: createSource() };

        function effect(type, details = {}) { return { type, ...details }; }

        function abortActive(source) {
            const state = sources[source];
            if (state.requestID === null) return [];
            const requestID = state.requestID;
            state.requestID = null;
            state.status = state.data === null ? "unavailable" : "fresh";
            state.error = null;
            return [effect("abortSource", { source, requestID })];
        }

        function requestSource(source, currentGeneration, host = selectedHost) {
            const requestID = nextRequestID++;
            const state = sources[source];
            state.status = "refreshing";
            state.error = null;
            state.requestID = requestID;
            state.generation = currentGeneration;
            const details = { source, requestID, generation: currentGeneration, window: selectedWindow };
            if (source === "trends") {
                details.host = host;
                details.queryWindow = selectedWindow;
                details.unfiltered = !host;
            }
            return effect("loadSource", details);
        }

        function startFullRefresh() {
            generation += 1;
            const effects = [effect("cancelRefresh")];
            effects.push(...abortActive("summary"), ...abortActive("trends"));
            fullGeneration = { id: generation, pending: new Set(["summary", "trends"]) };
            effects.push(requestSource("summary", generation), requestSource("trends", generation, knownHosts.length ? selectedHost : ""));
            return effects;
        }

        function settle(source, requestID, update) {
            const state = sources[source];
            if (!state || state.requestID !== requestID) return [];
            state.requestID = null;
            update(state);
            const effects = [effect("render")];
            if (fullGeneration && fullGeneration.pending.has(source)) {
                fullGeneration.pending.delete(source);
                if (fullGeneration.pending.size === 0) {
                    fullGeneration = null;
                    if (pageVisible) effects.push(effect("scheduleRefresh", { delayMs: refreshDelayMs }));
                }
            }
            return effects;
        }

        function hostNames(data) {
            const seen = new Set();
            return (Array.isArray(data && data.servers) ? data.servers : [])
                .map(server => String(server && server.name || "").trim())
                .filter(name => name && !seen.has(name) && seen.add(name))
                .sort((left, right) => left.localeCompare(right));
        }

        function dispatch(event = {}) {
            switch (event.type) {
                case "pageShown":
                    if (pageVisible) return [];
                    pageVisible = true;
                    return startFullRefresh();
                case "pageHidden": {
                    if (!pageVisible) return [];
                    pageVisible = false;
                    fullGeneration = null;
                    return [effect("cancelRefresh"), ...abortActive("summary"), ...abortActive("trends"), effect("render")];
                }
                case "manualRefresh":
                    return pageVisible ? startFullRefresh() : [];
                case "retrySource": {
                    const source = event.source === "trends" ? "trends" : "summary";
                    if (!pageVisible || sources[source].requestID !== null) return [];
                    generation += 1;
                    const retryHost = source === "trends" && knownHosts.length ? selectedHost : "";
                    return [requestSource(source, generation, retryHost)];
                }
                case "timerFired":
                    return pageVisible ? startFullRefresh() : [];
                case "windowChanged": {
                    const nextWindow = validWindows.has(event.window) ? event.window : "7d";
                    selectedWindow = nextWindow;
                    return pageVisible ? startFullRefresh() : [effect("render")];
                }
                case "hostChanged": {
                    selectedHost = String(event.host || "").trim();
                    page = 1;
                    if (!pageVisible) return [effect("render")];
                    generation += 1;
                    const effects = abortActive("trends");
                    effects.push(requestSource("trends", generation, selectedHost));
                    return effects;
                }
                case "filtersChanged":
                    search = String(event.search ?? search).trim();
                    attention = validAttentionFilters.has(event.attention) ? event.attention : attention;
                    sort = validSorts.has(event.sort) ? event.sort : sort;
                    page = 1;
                    return [effect("render")];
                case "pageChanged":
                    page = Math.max(1, Number.parseInt(event.page, 10) || 1);
                    return [effect("render")];
                case "sourceSucceeded": {
                    const effects = settle(event.source, event.requestID, state => {
                        if (event.timeZone) timeZone = String(event.timeZone);
                        state.status = "fresh";
                        state.data = clone(event.data);
                        state.error = null;
                        if (event.source === "trends" && event.unfiltered) {
                            knownHosts = hostNames(event.data);
                            if (selectedHost && !knownHosts.includes(selectedHost)) selectedHost = "";
                        }
                    });
                    if (!effects.length) return effects;
                    if (event.source === "trends") {
                        const collection = projectHealthCollection(sources.trends.data?.servers, {
                            search, attention, sort, window: selectedWindow, page, nowMS: now(),
                        });
                        page = collection.page;
                        if (event.unfiltered && selectedHost && knownHosts.includes(selectedHost)) {
                            generation += 1;
                            effects.push(requestSource("trends", generation, selectedHost));
                        }
                    }
                    return effects;
                }
                case "sourceFailed":
                    if (event.error && event.error.kind === "aborted") {
                        return settle(event.source, event.requestID, state => {
                            state.status = state.data === null ? "unavailable" : "fresh";
                            state.error = null;
                        });
                    }
                    return settle(event.source, event.requestID, state => {
                        state.status = state.data === null ? "unavailable" : "stale";
                        state.error = clone(event.error || { kind: "transport" });
                    });
                default:
                    return [];
            }
        }

        function getView() {
            const health = projectHealthCollection(sources.trends.data?.servers, {
                search, attention, sort, window: selectedWindow, page, nowMS: now(),
            });
            const projectTrendSeries = metric => selectedHost
                ? projectSelectedHostTrendSeries(
                    sources.trends.data?.servers,
                    selectedHost,
                    metric,
                    { window: selectedWindow, timeZone }
                )
                : projectFleetTrendSeries(
                    sources.trends.data?.servers,
                    metric,
                    { window: selectedWindow, timeZone }
                );
            const trendSeries = {
                packages: projectTrendSeries("packages"),
                security: projectTrendSeries("security"),
                disk: projectTrendSeries("disk"),
                failures: projectTrendSeries("failures"),
            };
            return clone({
                selectedWindow,
                selectedHost,
                timeZone,
                knownHosts,
                search,
                attention,
                sort,
                page,
                pageVisible,
                summary: sources.summary,
                trends: sources.trends,
                summaryLifecycle: projectSourceLifecycle("summary", sources.summary),
                trendsLifecycle: projectSourceLifecycle("trends", sources.trends),
                health,
                trendSeries,
                trendCharts: {
                    packages: projectTrendChart(trendSeries.packages, { integer: true }),
                    security: projectTrendChart(trendSeries.security, { integer: true }),
                    disk: projectTrendChart(trendSeries.disk),
                    failures: projectTrendChart(trendSeries.failures, { integer: true, minimumMax: 2 }),
                },
                refreshing: sources.summary.status === "refreshing" || sources.trends.status === "refreshing"
            });
        }

        return Object.freeze({ dispatch, getView });
    }

    return Object.freeze({
        createStore,
        projectFleetHealth,
        projectDurationConfidence,
        projectSourceLifecycle,
        projectHealthCollection,
        projectHealthTrendCSV,
        projectFleetTrendSeries,
        projectTrendChart,
        formatStorageKB,
    });
}));
