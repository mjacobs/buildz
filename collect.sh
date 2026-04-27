#!/usr/bin/env bash
# buildz hardware collector — runs on a target host, prints ONE JSON doc to stdout.
# Designed to be invoked via:  ssh host 'sudo bash -s' < collect.sh
#
# Requires on the target: bash, jq, and (ideally) lsblk, ip, dmidecode, smartctl, ethtool, lspci.
# Anything missing degrades gracefully — its section becomes null/empty.

set -uo pipefail

have() { command -v "$1" >/dev/null 2>&1; }

# Capture command stdout as a JSON string (or "null" if empty/missing).
cap_str() {
    local out
    out=$("$@" 2>/dev/null) || true
    if [[ -z "$out" ]]; then printf 'null'; else jq -Rs . <<<"$out"; fi
}

# Capture command stdout as raw JSON (assumed valid); "null" on failure.
cap_json() {
    local out
    out=$("$@" 2>/dev/null) || true
    if [[ -z "$out" ]]; then printf 'null'; else printf '%s' "$out"; fi
}

# --- basic identity ---------------------------------------------------------
HOSTNAME_VAL=$(hostname)
KERNEL=$(uname -r)
UNAME_A=$(uname -a)
OS_RELEASE_RAW=$(cat /etc/os-release 2>/dev/null || true)
UPTIME_SEC=$(awk '{print $1}' /proc/uptime 2>/dev/null || echo 0)

# --- raw text blobs (parsed driver-side) ------------------------------------
CPUINFO_RAW=$(cat /proc/cpuinfo 2>/dev/null || true)
MEMINFO_RAW=$(cat /proc/meminfo 2>/dev/null || true)

DMI_SYS_VENDOR=$(have dmidecode  && dmidecode -s system-manufacturer       2>/dev/null | head -1 || true)
DMI_SYS_PRODUCT=$(have dmidecode && dmidecode -s system-product-name       2>/dev/null | head -1 || true)
DMI_SYS_SERIAL=$(have dmidecode  && dmidecode -s system-serial-number      2>/dev/null | head -1 || true)
DMI_BOARD_VENDOR=$(have dmidecode  && dmidecode -s baseboard-manufacturer  2>/dev/null | head -1 || true)
DMI_BOARD_PRODUCT=$(have dmidecode && dmidecode -s baseboard-product-name  2>/dev/null | head -1 || true)
DMI_BOARD_SERIAL=$(have dmidecode  && dmidecode -s baseboard-serial-number 2>/dev/null | head -1 || true)
DMI_BIOS_VENDOR=$(have dmidecode   && dmidecode -s bios-vendor             2>/dev/null | head -1 || true)
DMI_BIOS_VERSION=$(have dmidecode  && dmidecode -s bios-version            2>/dev/null | head -1 || true)
DMI_MEMORY_RAW=$(have dmidecode    && dmidecode -t memory                  2>/dev/null || true)

# --- structured tools -------------------------------------------------------
LSBLK_JSON=$(have lsblk && cap_json lsblk -J -b -O || echo null)
IP_ADDR_JSON=$(have ip   && cap_json ip -j -d addr  || echo null)
LSPCI_RAW=$(have lspci   && lspci -vmmnn 2>/dev/null || true)

# Per-NIC ethtool, keyed by ifname (non-loopback only).
ETHTOOL_OBJ='{}'
if have ip && have ethtool; then
    while read -r ifname; do
        [[ -z "$ifname" || "$ifname" == "lo" ]] && continue
        data=$(ethtool "$ifname" 2>/dev/null || true)
        [[ -z "$data" ]] && continue
        ETHTOOL_OBJ=$(jq --arg k "$ifname" --arg v "$data" '. + {($k): $v}' <<<"$ETHTOOL_OBJ")
    done < <(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | awk '{print $1}')
fi

# Per-disk smartctl. Skip loop/zram/dm devices.
SMART_ARR="[]"
if have lsblk && have smartctl; then
    SMART_ARR="["
    first=1
    while read -r dev; do
        [[ -z "$dev" ]] && continue
        case "$dev" in loop*|zram*|dm-*|sr*) continue ;; esac
        data=$(smartctl -a --json /dev/"$dev" 2>/dev/null || true)
        [[ -z "$data" ]] && data="null"
        [[ $first -eq 0 ]] && SMART_ARR+=","
        SMART_ARR+="{\"device\":\"/dev/$dev\",\"data\":$data}"
        first=0
    done < <(lsblk -dno NAME,TYPE 2>/dev/null | awk '$2=="disk"{print $1}')
    SMART_ARR+="]"
fi

NVIDIA_CSV=""
if have nvidia-smi; then
    NVIDIA_CSV=$(nvidia-smi --query-gpu=name,pci.bus_id,memory.total --format=csv,noheader 2>/dev/null || true)
fi

# --- emit -------------------------------------------------------------------
jq -n \
    --arg     hostname        "$HOSTNAME_VAL" \
    --arg     kernel          "$KERNEL" \
    --arg     uname           "$UNAME_A" \
    --arg     os_release_raw  "$OS_RELEASE_RAW" \
    --argjson uptime_seconds  "${UPTIME_SEC%.*}" \
    --arg     cpuinfo_raw     "$CPUINFO_RAW" \
    --arg     meminfo_raw     "$MEMINFO_RAW" \
    --arg     dmi_sys_vendor    "$DMI_SYS_VENDOR" \
    --arg     dmi_sys_product   "$DMI_SYS_PRODUCT" \
    --arg     dmi_sys_serial    "$DMI_SYS_SERIAL" \
    --arg     dmi_board_vendor  "$DMI_BOARD_VENDOR" \
    --arg     dmi_board_product "$DMI_BOARD_PRODUCT" \
    --arg     dmi_board_serial  "$DMI_BOARD_SERIAL" \
    --arg     dmi_bios_vendor   "$DMI_BIOS_VENDOR" \
    --arg     dmi_bios_version  "$DMI_BIOS_VERSION" \
    --arg     dmi_memory_raw    "$DMI_MEMORY_RAW" \
    --argjson lsblk           "$LSBLK_JSON" \
    --argjson ip_addr         "$IP_ADDR_JSON" \
    --arg     lspci_raw       "$LSPCI_RAW" \
    --argjson ethtool         "$ETHTOOL_OBJ" \
    --argjson smart           "$SMART_ARR" \
    --arg     nvidia_csv      "$NVIDIA_CSV" \
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
    lsblk: $lsblk,
    ip_addr: $ip_addr,
    lspci_raw: $lspci_raw,
    ethtool: $ethtool,
    smart: $smart,
    nvidia_csv: $nvidia_csv
}'
