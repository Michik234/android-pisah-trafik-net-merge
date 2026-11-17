package org.netmerge.android

import org.json.JSONArray

/** One connection in the live monitor (netmerge.Netmerge.flows()). */
data class FlowRow(
    val id: Long,
    val proto: String, // "tcp" or "udp"
    val ip: String,
    val port: Int,
    val network: String,
    val active: Boolean,
    /** Matches the split-mode game ports. If [network] is Wi-Fi anyway, it fell back. */
    val game: Boolean,
    val bytesIn: Long,
    val bytesOut: Long,
    val startedMs: Long,
    val lastMs: Long,
    val endedMs: Long,
    /** The app that owns the connection, when known (several packages are joined with "|"). */
    val process: String,
    /** The rule that decided it, such as "rule 2: MOBILE". */
    val rule: String,
) {
    val isUdp: Boolean get() = proto == "udp"

    /** "93.184.216.34:443", or "[2001:db8::1]:443" for IPv6. */
    val address: String get() = if (ip.contains(':')) "[$ip]:$port" else "$ip:$port"

    companion object {
        fun parse(json: String): List<FlowRow> {
            val arr = runCatching { JSONArray(json) }.getOrNull() ?: return emptyList()
            return (0 until arr.length()).map { i ->
                val o = arr.getJSONObject(i)
                FlowRow(
                    id = o.optLong("id"),
                    proto = o.optString("proto"),
                    ip = o.optString("ip"),
                    port = o.optInt("port"),
                    network = o.optString("net"),
                    active = o.optBoolean("active"),
                    game = o.optBoolean("game"),
                    bytesIn = o.optLong("in"),
                    bytesOut = o.optLong("out"),
                    startedMs = o.optLong("started"),
                    lastMs = o.optLong("last"),
                    endedMs = o.optLong("ended"),
                    process = o.optString("process"),
                    rule = o.optString("rule"),
                )
            }
        }

        /** "5s", "3m 12s", "1h 04m". */
        fun duration(ms: Long): String {
            val s = (ms / 1000).coerceAtLeast(0)
            return when {
                s < 60 -> "${s}s"
                s < 3600 -> "${s / 60}m ${"%02d".format(s % 60)}s"
                else -> "${s / 3600}h ${"%02d".format(s % 3600 / 60)}m"
            }
        }
    }
}
