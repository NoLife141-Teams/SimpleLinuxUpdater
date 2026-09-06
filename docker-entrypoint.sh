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
    remaining="$(trim_space "$1")"
    while [ -n "$remaining" ]; do
        case "$remaining" in
            *:*)
                candidate="${remaining%%:*}"
                remaining="${remaining#*:}"
                ;;
            *)
                candidate="$remaining"
                remaining=""
                ;;
        esac
        candidate="$(trim_space "$candidate")"
        if [ -n "$candidate" ]; then
            printf '%s\n' "$candidate"
            return 0
        fi
    done
    return 1
}

assert_no_symlink_components() {
    path="$1"
    if [ -z "$path" ]; then
        fail_path "refusing empty persistence path"
    fi

    case "$path" in
        /*)
            current=""
            remaining="${path#/}"
            ;;
        *)
            current="."
            remaining="$path"
            ;;
    esac

    while [ -n "$remaining" ]; do
        case "$remaining" in
            */*)
                component="${remaining%%/*}"
                remaining="${remaining#*/}"
                ;;
            *)
                component="$remaining"
                remaining=""
                ;;
        esac

        if [ -z "$component" ] || [ "$component" = "." ]; then
            continue
        fi
        if [ "$component" = ".." ]; then
            fail_path "refusing persistence path containing '..': $path"
        fi

        if [ -z "$current" ]; then
            current="/$component"
        else
            current="$current/$component"
        fi
        if [ -L "$current" ]; then
            fail_path "refusing symbolic link in persistence path: $current"
        fi
    done
}

chown_data_directory_chain() {
    dir="$1"
    case "$dir" in
        /data)
            current="/data"
            remaining=""
            ;;
        /data/*)
            current="/data"
            remaining="${dir#/data/}"
            ;;
        data)
            current="data"
            remaining=""
            ;;
        data/*)
            current="data"
            remaining="${dir#data/}"
            ;;
        *)
            return 1
            ;;
    esac

    while :; do
        if [ ! -d "$current" ]; then
            fail_path "expected persistence directory, found non-directory: $current"
        fi
        chown -h "$app_user:$app_group" "$current"
        if [ -z "$remaining" ]; then
            return 0
        fi
        case "$remaining" in
            */*)
                component="${remaining%%/*}"
                remaining="${remaining#*/}"
                ;;
            *)
                component="$remaining"
                remaining=""
                ;;
        esac
        current="$current/$component"
    done
}

ensure_owned_directory() {
    dir="$1"
    if [ "$dir" = "/" ]; then
        fail_path "refusing to change ownership of filesystem root"
    fi
    assert_no_symlink_components "$dir"

    if [ -e "$dir" ]; then
        if [ ! -d "$dir" ]; then
            fail_path "expected persistence directory, found non-directory: $dir"
        fi
    else
        mkdir -p "$dir"
        # Re-check after creation so every existing component is validated before root chown.
        assert_no_symlink_components "$dir"
    fi

    # Under the Docker persistence root, repair only the directory chain needed
    # to reach the configured target. This replaces the old recursive /data chown
    # without leaving root-owned 0700 ancestors from earlier images.
    if chown_data_directory_chain "$dir"; then
        return
    fi

    # -h is defense in depth against a final-component symlink swap.
    chown -h "$app_user:$app_group" "$dir"
}

chown_regular_file_if_present() {
    path="$1"
    assert_no_symlink_components "$path"

    if [ ! -e "$path" ]; then
        return
    fi
    if [ ! -f "$path" ]; then
        fail_path "expected regular persistence file: $path"
    fi

    # Never dereference a symlink if the path changes after validation.
    chown -h "$app_user:$app_group" "$path"
}

prepare_persistence_file() {
    path="$1"
    dir="$(dirname "$path")"
    ensure_owned_directory "$dir"
    chown_regular_file_if_present "$path"
}

prepare_data_dir() {
    dir="$1"
    db_file="$2"

    if [ -z "$dir" ]; then
        return
    fi

    ensure_owned_directory "$dir"
    chown_regular_file_if_present "$db_file"
    chown_regular_file_if_present "$db_file-wal"
    chown_regular_file_if_present "$db_file-shm"
    chown_regular_file_if_present "$dir/config.json"
}

path_within_directory() {
    path="$1"
    dir="$2"
    case "$dir" in
        /)
            return 1
            ;;
        .)
            case "$path" in
                /*) return 1 ;;
                *) return 0 ;;
            esac
            ;;
        *)
            case "$path" in
                "$dir"/*) return 0 ;;
                *) return 1 ;;
            esac
            ;;
    esac
}

should_repair_known_hosts() {
    path="$1"
    db_dir="$2"
    case "$path" in
        /data/*|data/*)
            return 0
            ;;
    esac
    path_within_directory "$path" "$db_dir"
}

if [ "$(id -u)" = "0" ]; then
    # Match the application's strings.TrimSpace behavior for operator-supplied paths.
    db_path="$(trim_space "${DEBIAN_UPDATER_DB_PATH:-}")"
    if [ -z "$db_path" ]; then
        db_path="/data/servers.db"
    fi
    db_dir="$(dirname "$db_path")"

    # Previous images wrote persisted files as root. Repair only known runtime
    # paths and the required directory chain; never recursively chown app-writable files.
    if [ -e /data ] || [ -L /data ]; then
        ensure_owned_directory "/data"
        chown_regular_file_if_present "/data/servers.json"
    fi
    prepare_data_dir "$db_dir" "$db_path"

    known_hosts_path=""
    known_hosts_configured=0
    if configured_known_hosts="$(first_path_list_entry "${DEBIAN_UPDATER_KNOWN_HOSTS:-}")"; then
        known_hosts_path="$configured_known_hosts"
        known_hosts_configured=1
    else
        known_hosts_path="$db_dir/known_hosts"
    fi

    # A configured write target is repaired only when it is inside the Docker
    # persistence tree or the already-owned DB directory. Never chown system
    # locations such as /etc/ssh merely because they were listed for reading.
    if [ "$known_hosts_configured" = "0" ] || should_repair_known_hosts "$known_hosts_path" "$db_dir"; then
        prepare_persistence_file "$known_hosts_path"
    fi

    exec su-exec "$app_user:$app_group" "$@"
fi

exec "$@"
