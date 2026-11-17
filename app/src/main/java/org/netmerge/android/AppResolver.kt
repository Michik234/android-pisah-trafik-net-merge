package org.netmerge.android

import android.content.Context
import android.net.ConnectivityManager
import android.os.Build
import android.system.OsConstants
import java.net.InetAddress
import java.net.InetSocketAddress
import java.util.concurrent.ConcurrentHashMap

/**
 * Tells the engine which app owns a connection, for PROCESS-NAME rules and the live list. Uses
 * ConnectivityManager.getConnectionOwnerUid, which a VPN app may call for its own tunnel (Android 10+;
 * on older versions the answer is always "unknown").
 *
 * The engine calls this from its own threads for each new connection, so nothing here may throw: an
 * exception crossing into Go would kill the process.
 */
class AppResolver(context: Context) : netmerge.ProcessResolver {
    private val cm = context.getSystemService(ConnectivityManager::class.java)
    private val pm = context.packageManager
    private val names = ConcurrentHashMap<Int, String>()

    override fun name(udp: Boolean, srcIP: String, srcPort: Long, dstIP: String, dstPort: Long): String {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) return ""
        return try {
            val uid = cm.getConnectionOwnerUid(
                if (udp) OsConstants.IPPROTO_UDP else OsConstants.IPPROTO_TCP,
                InetSocketAddress(InetAddress.getByName(srcIP), srcPort.toInt()),
                InetSocketAddress(InetAddress.getByName(dstIP), dstPort.toInt()),
            )
            if (uid < 0) "" else names[uid] ?: packages(uid).also { if (it.isNotEmpty()) names[uid] = it }
        } catch (e: Throwable) {
            ""
        }
    }

    /** Every package of the uid, joined with "|" (apps can share a uid). */
    private fun packages(uid: Int): String {
        val list = pm.getPackagesForUid(uid)?.toList().orEmpty()
        return if (list.isNotEmpty()) list.joinToString("|") else pm.getNameForUid(uid).orEmpty()
    }
}
