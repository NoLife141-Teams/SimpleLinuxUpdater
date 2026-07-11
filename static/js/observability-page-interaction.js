(function initObservabilityPageInteraction(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.ObservabilityPageInteraction = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function observabilityPageInteractionFactory() {
    "use strict";

    const validWindows = new Set(["24h", "7d", "30d", "90d"]);

    function clone(value) {
        if (Array.isArray(value)) return value.map(clone);
        if (!value || typeof value !== "object") return value;
        return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, clone(item)]));
    }

    function createSource() {
        return { status: "unavailable", data: null, error: null, requestID: null, generation: 0 };
    }

    function createStore(options = {}) {
        const refreshDelayMs = Number(options.refreshDelayMs) > 0 ? Number(options.refreshDelayMs) : 15000;
        let selectedWindow = validWindows.has(options.window) ? options.window : "7d";
        let selectedHost = "";
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
                details.queryWindow = selectedWindow === "24h" ? "7d" : selectedWindow;
                details.unfiltered = !host;
            }
            return effect("loadSource", details);
        }

        function startFullRefresh() {
            generation += 1;
            const effects = [effect("cancelRefresh")];
            effects.push(...abortActive("summary"), ...abortActive("trends"));
            fullGeneration = { id: generation, pending: new Set(["summary", "trends"]) };
            effects.push(requestSource("summary", generation), requestSource("trends", generation));
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
                case "timerFired":
                    return pageVisible ? startFullRefresh() : [];
                case "windowChanged": {
                    const nextWindow = validWindows.has(event.window) ? event.window : "7d";
                    selectedWindow = nextWindow;
                    return pageVisible ? startFullRefresh() : [effect("render")];
                }
                case "hostChanged": {
                    selectedHost = String(event.host || "").trim();
                    if (!pageVisible) return [effect("render")];
                    generation += 1;
                    const effects = abortActive("trends");
                    effects.push(requestSource("trends", generation, selectedHost));
                    return effects;
                }
                case "sourceSucceeded":
                    return settle(event.source, event.requestID, state => {
                        state.status = "fresh";
                        state.data = clone(event.data);
                        state.error = null;
                        if (event.source === "trends" && event.unfiltered) {
                            knownHosts = hostNames(event.data);
                            if (selectedHost && !knownHosts.includes(selectedHost)) selectedHost = "";
                        }
                    });
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
            return clone({
                selectedWindow,
                selectedHost,
                knownHosts,
                pageVisible,
                summary: sources.summary,
                trends: sources.trends,
                refreshing: sources.summary.status === "refreshing" || sources.trends.status === "refreshing"
            });
        }

        return Object.freeze({ dispatch, getView });
    }

    return Object.freeze({ createStore });
}));
