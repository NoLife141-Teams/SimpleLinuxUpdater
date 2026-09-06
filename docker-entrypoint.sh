#!/bin/sh
set -eu

app_user="app"
app_group="app"

fail_path() {
    printf 'docker-entrypoint: %s\n' "$*" >&2
    exit 1
}

trim_space() {
    printf '%s' "$1" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//'
}

first_path_list_entry() {
    fple_remaining="$(trim_space "$1")"
    while [ -n "$fple_remaining" ]; do
        case "$fple_remaining" in
            *:*)
                fple_candidate="${fple_remaining%%:*}"
                fple_remaining="${fple_remaining#*:}"
                ;;
            *)
                fple_candidate="$fple_remaining"
                fple_remaining=""
                ;;
        esac
        fple_candidate="$(trim_space "$fple_candidate")"
        if [ -n "$fple_candidate" ]; then
            printf '%s\n' "$fple_candidate"
            return 0
        fi
    done
    return 1
}

assert_no_symlink_components() {
    ansc_path="$1"
    if [ -z "$ansc_path" ]; then
        fail_path "refusing empty persistence path"
    fi

    case "$ansc_path" in
        /*)
            ansc_current=""
            ansc_remaining="${ansc_path#/}"
            ;;
        *)
            ansc_current="."
            ansc_remaining="$ansc_path"
            ;;
    esac

    while [ -n "$ansc_remaining" ]; do
        case "$ansc_remaining" in
            */*)
                ansc_component="${ansc_remaining%%/*}"
                ansc_remaining="${ansc_remaining#*/}"
                ;;
            *)
                ansc_component="$ansc_remaining"
                ansc_remaining=""
                ;;
        esac

        if [ -z "$ansc_component" ] || [ "$ansc_component" = "." ]; then
            continue
        fi
        if [ "$ansc_component" = ".." ]; then
            fail_path "refusing persistence path containing '..': $ansc_path"
        fi

        if [ -z "$ansc_current" ]; then
            ansc_current="/$ansc_component"
        else
            ansc_current="$ansc_current/$ansc_component"
        fi
        if [ -L "$ansc_current" ]; then
            fail_path "refusing symbolic link in persistence path: $ansc_current"
        fi
    done
}

chown_data_directory_chain() {
    cddc_dir="$1"
    case "$cddc_dir" in
        /data)
            cddc_current="/data"
            cddc_remaining=""
            ;;
        /data/*)
            cddc_current="/data"
            cddc_remaining="${cddc_dir#/data/}"
            ;;
        data)
            cddc_current="data"
            cddc_remaining=""
            ;;
        data/*)
            cddc_current="data"
            cddc_remaining="${cddc_dir#data/}"
            ;;
        *)
            return 1
            ;;
    esac

    while :; do
        if [ ! -d "$cddc_current" ]; then
            fail_path "expected persistence directory, found non-directory: $cddc_current"
        fi
        chown -h "$app_user:$app_group" "$cddc_current"
        if [ -z "$cddc_remaining" ]; then
            return 0
        fi
        case "$cddc_remaining" in
            */*)
                cddc_component="${cddc_remaining%%/*}"
                cddc_remaining="${cddc_remaining#*/}"
                ;;
            *)
                cddc_component="$cddc_remaining"
                cddc_remaining=""
                ;;
        esac
        cddc_current="$cddc_current/$cddc_component"
    done
}

ensure_owned_directory() {
    eod_dir="$1"
    if [ "$eod_dir" = "/" ]; then
        fail_path "refusing to change ownership of filesystem root"
    fi
    assert_no_symlink_components "$eod_dir"

    if [ -e "$eod_dir" ]; then
        if [ ! -d "$eod_dir" ]; then
            fail_path "expected persistence directory, found non-directory: $eod_dir"
        fi
    else
        mkdir -p "$eod_dir"
        # Re-check after creation so every existing component is validated before root chown.
        assert_no_symlink_components "$eod_dir"
    fi

    # Under the Docker persistence root, repair only the directory chain needed
    # to reach the configured target. This replaces the old recursive /data chown
    # without leaving root-owned 0700 ancestors from earlier images.
    if chown_data_directory_chain "$eod_dir"; then
        return
    fi

    # -h is defense in depth against a final-component symlink swap.
    chown -h "$app_user:$app_group" "$eod_dir"
}

chown_regular_file_if_present() {
    crfip_path="$1"
    assert_no_symlink_components "$crfip_path"

    if [ ! -e "$crfip_path" ]; then
        return
    fi
    if [ ! -f "$crfip_path" ]; then
        fail_path "expected regular persistence file: $crfip_path"
    fi

    # Never dereference a symlink if the path changes after validation.
    chown -h "$app_user:$app_group" "$crfip_path"
}

prepare_persistence_file() {
    ppf_path="$1"
    ppf_dir="$(dirname "$ppf_path")"
    ensure_owned_directory "$ppf_dir"
    chown_regular_file_if_present "$ppf_path"
}

prepare_data_dir() {
    pdd_dir="$1"
    pdd_db_file="$2"

    if [ -z "$pdd_dir" ]; then
        return
    fi

    ensure_owned_directory "$pdd_dir"
    chown_regular_file_if_present "$pdd_db_file"
    chown_regular_file_if_present "$pdd_db_file-wal"
    chown_regular_file_if_present "$pdd_db_file-shm"
    chown_regular_file_if_present "$pdd_dir/config.json"
}

path_within_directory() {
    pwd_path="$1"
    pwd_dir="$2"
    case "$pwd_dir" in
        /)
            return 1
            ;;
        .)
            case "$pwd_path" in
                /*) return 1 ;;
                *) return 0 ;;
            esac
            ;;
        *)
            case "$pwd_path" in
                "$pwd_dir"/*) return 0 ;;
                *) return 1 ;;
            esac
            ;;
    esac
}

should_repair_known_hosts() {
    srkh_path="$1"
    srkh_db_dir="$2"
    case "$srkh_path" in
        /data/*|data/*)
            return 0
            ;;
    esac
    path_within_directory "$srkh_path" "$srkh_db_dir"
}

if [ "$(id -u)" = "0" ]; then
    # Match the application's strings.TrimSpace behavior for operator-supplied paths.
    entry_db_path="$(trim_space "${DEBIAN_UPDATER_DB_PATH:-}")"
    if [ -z "$entry_db_path" ]; then
        entry_db_path="/data/servers.db"
    fi
    entry_db_dir="$(dirname "$entry_db_path")"

    # Previous images wrote persisted files as root. Repair only known runtime
    # paths and the required directory chain; never recursively chown app-writable files.
    if [ -e /data ] || [ -L /data ]; then
        ensure_owned_directory "/data"
        chown_regular_file_if_present "/data/servers.json"
    fi
    prepare_data_dir "$entry_db_dir" "$entry_db_path"

    entry_known_hosts_path=""
    entry_known_hosts_configured=0
    if entry_configured_known_hosts="$(first_path_list_entry "${DEBIAN_UPDATER_KNOWN_HOSTS:-}")"; then
        entry_known_hosts_path="$entry_configured_known_hosts"
        entry_known_hosts_configured=1
    else
        entry_known_hosts_path="$entry_db_dir/known_hosts"
    fi

    # A configured write target is repaired only when it is inside the Docker
    # persistence tree or the already-owned DB directory. Never chown system
    # locations such as /etc/ssh merely because they were listed for reading.
    if [ "$entry_known_hosts_configured" = "0" ] || should_repair_known_hosts "$entry_known_hosts_path" "$entry_db_dir"; then
        prepare_persistence_file "$entry_known_hosts_path"
    fi

    exec su-exec "$app_user:$app_group" "$@"
fi

exec "$@"
