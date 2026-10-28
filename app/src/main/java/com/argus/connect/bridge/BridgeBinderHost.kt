package com.argus.connect.bridge

import android.content.Context
import android.os.Binder
import android.os.Bundle
import android.os.IBinder
import android.os.Parcel
import android.os.Parcelable
import kotlin.system.exitProcess

/** App-owned IPC over Binder: Android rejects shell↔app abstract UNIX sockets. */
@android.annotation.TargetApi(36)
internal class BridgeBinderHost(private val context: Context, packageName: String) : Binder() {
    private val appUid = context.packageManager.getApplicationInfo(packageName, 0).uid
    private val authority = "$packageName.bridge"
    private val platform = PlatformBridge(context)
    private val lock = Any()
    private val providerToken = Binder()

    override fun onTransact(code: Int, data: Parcel, reply: Parcel?, flags: Int): Boolean {
        if (code != 1 || reply == null) return super.onTransact(code, data, reply, flags)
        check(getCallingUid() == appUid) { "only owner UID may control bridge" }
        data.enforceInterface("com.argus.connect.bridge.v9")
        val command = data.readString()
        // Work is authorized above, then execute platform calls as shell, not
        // with the caller app's unprivileged Binder identity.
        val identity = clearCallingIdentity()
        try {
            synchronized(lock) {
                try {
                    val prepared = if (command == "PREPARE") platform.prepare() else null
                    val result = when (command) {
                        "PROBE" -> platform.probe()
                        "PREPARE" -> "TUN|${prepared!!.interfaceName}"
                        "ACTIVATE" -> platform.activate()
                        "STATUS" -> platform.status()
                        "STOP", "TEARDOWN", "SHUTDOWN" -> { platform.stop(); "OK|stopped" }
                        else -> error("unknown command")
                    }
                    reply.writeNoException()
                    reply.writeString(result)
                    reply.writeTypedObject(prepared?.descriptor, 0)
                } catch (failure: Throwable) {
                    runCatching { platform.stop() }
                    var root = failure
                    while (root.cause != null && root.cause !== root) root = root.cause!!
                    reply.writeNoException()
                    reply.writeString("ERROR|${root.javaClass.simpleName}: ${root.message}")
                    reply.writeTypedObject<android.os.ParcelFileDescriptor>(null, 0)
                }
            }
        } finally { restoreCallingIdentity(identity) }
        return true
    }

    fun publish() {
        val am = Class.forName("android.app.ActivityManager").getMethod("getService").invoke(null)
        val acquire = am.javaClass.methods.first { it.name == "getContentProviderExternal" && it.parameterTypes.size == 4 }
        val holder = acquire.invoke(am, authority, 0, providerToken, "argus-bridge")
            ?: error("bridge provider not found")
        val provider = holder.javaClass.getField("provider").get(holder)
        val call = provider.javaClass.methods.first { it.name == "call" && it.parameterTypes.size == 5 }
        val result = call.invoke(provider, context.attributionSource, authority, "publish", "9",
            Bundle().apply { putBinder("helper", this@BridgeBinderHost) }) as Bundle
        val owner = result.getBinder("owner") ?: error("no app lifetime binder")
        owner.linkToDeath({
            synchronized(lock) { runCatching { platform.stop() } }
            exitProcess(0)
        }, 0)
        println("ARGUS_BINDER_READY|protocol=9")
        System.out.flush()
    }
}
