#!/usr/bin/env bash
# buildz hardware collector — runs on a target host, prints ONE JSON doc to stdout.
# Designed to be invoked via:  ssh host 'sudo bash -s' < collect.sh
#
# Requires on the target: bash, jq, and (ideally) lsblk, ip, dmidecode, smartctl,
# ethtool, lspci. Anything missing degrades gracefully — its section becomes null/empty.
#
# Memory note: large blobs (lsblk JSON, smartctl JSON, dmidecode -t memory output)
# are staged to a tmp dir and read into the final jq via --rawfile / --slurpfile,
# rather than expanded into a single giant argv. This matters on memory-tight
# hosts (e.g. TrueNAS with many disks).

set -uo pipefail

have() { command -v "$1" >/dev/null 2>&1; }

TMPDIR=$(mktemp -d "${TMPDIR:-/tmp}/buildz.XXXXXX")
trap 'rm -rf "$TMPDIR"' EXIT

# Capture stdout of "$@" into file $1, swallowing errors. Empty-on-failure.
cap_to() {
    local out=$1; shift
    "$@" >"$out" 2>/dev/null || true
}

# Same, but ensures the file contains valid JSON: writes "null" on failure/empty.
cap_json_to() {
    local out=$1; shift
    if "$@" >"$out" 2>/dev/null && [[ -s "$out" ]]; then :; else
        printf 'null' >"$out"
    fi
}

# --- basic identity ---------------------------------------------------------
HOSTNAME_VAL=$(hostname)
KERNEL=$(uname -r)
UNAME_A=$(uname -a)
UPTIME_SEC=$(awk '{print $1}' /proc/uptime 2>/dev/null || echo 0)
UPTIME_INT=${UPTIME_SEC%.*}
: "${UPTIME_INT:=0}"

# --- raw text blobs (parsed driver-side) ------------------------------------
cap_to "$TMPDIR/cpuinfo"        cat /proc/cpuinfo
cap_to "$TMPDIR/meminfo"        cat /proc/meminfo
cap_to "$TMPDIR/os_release"     cat /etc/os-release

if have dmidecode; then
    DMI_SYS_VENDOR=$(dmidecode -s system-manufacturer       2>/dev/null | head -1 || true)
    DMI_SYS_PRODUCT=$(dmidecode -s system-product-name      2>/dev/null | head -1 || true)
    DMI_SYS_SERIAL=$(dmidecode -s system-serial-number      2>/dev/null | head -1 || true)
    DMI_BOARD_VENDOR=$(dmidecode -s baseboard-manufacturer  2>/dev/null | head -1 || true)
    DMI_BOARD_PRODUCT=$(dmidecode -s baseboard-product-name 2>/dev/null | head -1 || true)
    DMI_BOARD_SERIAL=$(dmidecode -s baseboard-serial-number 2>/dev/null | head -1 || true)
    DMI_BIOS_VENDOR=$(dmidecode -s bios-vendor              2>/dev/null | head -1 || true)
    DMI_BIOS_VERSION=$(dmidecode -s bios-version            2>/dev/null | head -1 || true)
    cap_to "$TMPDIR/dmi_memory" dmidecode -t memory
else
    DMI_SYS_VENDOR= DMI_SYS_PRODUCT= DMI_SYS_SERIAL= ; DMI_BOARD_VENDOR= DMI_BOARD_PRODUCT= DMI_BOARD_SERIAL= ; DMI_BIOS_VENDOR= DMI_BIOS_VERSION=
    : >"$TMPDIR/dmi_memory"
fi

# --- structured tools -------------------------------------------------------
if have lsblk;  then cap_json_to "$TMPDIR/lsblk.json"  lsblk -J -b -O; else printf 'null' >"$TMPDIR/lsblk.json"; fi
if have ip;     then cap_json_to "$TMPDIR/ip_addr.json" ip -j -d addr; else printf 'null' >"$TMPDIR/ip_addr.json"; fi
if have lspci;  then cap_to       "$TMPDIR/lspci"        lspci -vmmnn;  else : >"$TMPDIR/lspci"; fi

# Per-NIC ethtool, keyed by ifname. Build incrementally on disk.
printf '{}' >"$TMPDIR/ethtool.json"
if have ip && have ethtool; then
    while read -r ifname; do
        [[ -z "$ifname" || "$ifname" == "lo" ]] && continue
        printf '%s' "$(ethtool "$ifname" 2>/dev/null || true)" >"$TMPDIR/_ethtool_one"
        [[ -s "$TMPDIR/_ethtool_one" ]] || continue
        jq --arg k "$ifname" --rawfile v "$TMPDIR/_ethtool_one" \
           '. + {($k): $v}' "$TMPDIR/ethtool.json" >"$TMPDIR/_ethtool_next"
        mv "$TMPDIR/_ethtool_next" "$TMPDIR/ethtool.json"
    done < <(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | awk '{print $1}')
fi

# Per-disk smartctl. One JSON object per line in _smart_parts; merged into an array.
: >"$TMPDIR/_smart_parts"
if have lsblk && have smartctl; then
    while read -r dev; do
        [[ -z "$dev" ]] && continue
        case "$dev" in loop*|zram*|dm-*|sr*) continue ;; esac
        smartctl -a --json /dev/"$dev" 2>/dev/null >"$TMPDIR/_smart_one" || true
        [[ -s "$TMPDIR/_smart_one" ]] || printf 'null' >"$TMPDIR/_smart_one"
        jq -c -n --arg d "/dev/$dev" --slurpfile data "$TMPDIR/_smart_one" \
            '{device:$d, data:$data[0]}' >>"$TMPDIR/_smart_parts"
    done < <(lsblk -dno NAME,TYPE 2>/dev/null | awk '$2=="disk"{print $1}')
fi
if [[ -s "$TMPDIR/_smart_parts" ]]; then
    jq -s '.' "$TMPDIR/_smart_parts" >"$TMPDIR/smart.json"
else
    printf '[]' >"$TMPDIR/smart.json"
fi

NVIDIA_CSV=
if have nvidia-smi; then
    NVIDIA_CSV=$(nvidia-smi --query-gpu=name,pci.bus_id,memory.total --format=csv,noheader 2>/dev/null || true)
fi

# --- emit -------------------------------------------------------------------
# Big blobs come in via --rawfile / --slurpfile so we don't blow argv on memory-
# tight hosts. Small fields stay as --arg.
jq -n \
    --arg       hostname          "$HOSTNAME_VAL" \
    --arg       kernel            "$KERNEL" \
    --arg       uname             "$UNAME_A" \
    --argjson   uptime_seconds    "$UPTIME_INT" \
    --rawfile   cpuinfo_raw       "$TMPDIR/cpuinfo" \
    --rawfile   meminfo_raw       "$TMPDIR/meminfo" \
    --rawfile   os_release_raw    "$TMPDIR/os_release" \
    --rawfile   lspci_raw         "$TMPDIR/lspci" \
    --rawfile   dmi_memory_raw    "$TMPDIR/dmi_memory" \
    --slurpfile lsblk             "$TMPDIR/lsblk.json" \
    --slurpfile ip_addr           "$TMPDIR/ip_addr.json" \
    --slurpfile ethtool           "$TMPDIR/ethtool.json" \
    --slurpfile smart             "$TMPDIR/smart.json" \
    --arg       dmi_sys_vendor    "$DMI_SYS_VENDOR" \
    --arg       dmi_sys_product   "$DMI_SYS_PRODUCT" \
    --arg       dmi_sys_serial    "$DMI_SYS_SERIAL" \
    --arg       dmi_board_vendor  "$DMI_BOARD_VENDOR" \
    --arg       dmi_board_product "$DMI_BOARD_PRODUCT" \
    --arg       dmi_board_serial  "$DMI_BOARD_SERIAL" \
    --arg       dmi_bios_vendor   "$DMI_BIOS_VENDOR" \
    --arg       dmi_bios_version  "$DMI_BIOS_VERSION" \
    --arg       nvidia_csv        "$NVIDIA_CSV" \
'{
    schema_version: 1,
    collected_at: (now | todate),
    hostname: $hostname,
    kernel: $kernel,
    uname: $uname,
    os_release_raw: $os_release_raw,
    uptime_seconds: $uptime_seconds,
    cpuinfo_raw: $cpuinfo_raw,
    meminfo_raw: $meminfo_raw,
    dmi: {
        system_vendor:    $dmi_sys_vendor,
        system_product:   $dmi_sys_product,
        system_serial:    $dmi_sys_serial,
        board_vendor:     $dmi_board_vendor,
        board_product:    $dmi_board_product,
        board_serial:     $dmi_board_serial,
        bios_vendor:      $dmi_bios_vendor,
        bios_version:     $dmi_bios_version,
        memory_raw:       $dmi_memory_raw
    },
    lsblk:     $lsblk[0],
    ip_addr:   $ip_addr[0],
    lspci_raw: $lspci_raw,
    ethtool:   $ethtool[0],
    smart:     $smart[0],
    nvidia_csv: $nvidia_csv
}'
