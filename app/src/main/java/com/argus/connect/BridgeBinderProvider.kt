package com.argus.connect

import android.content.ContentProvider
import android.content.ContentValues
import android.database.Cursor
import android.net.Uri
import android.os.Binder
import android.os.Bundle
import android.os.IBinder

internal object BridgeBinderRegistry {
    @Volatile var helper: IBinder? = null
    val owner = Binder()
}

/** Bootstrap only: the OS enforces DUMP and we additionally require shell UID. */
class BridgeBinderProvider : ContentProvider() {
    override fun onCreate() = true
    override fun call(method: String, arg: String?, extras: Bundle?): Bundle {
        check(Binder.getCallingUid() == 2000) { "shell UID required" }
        check(method == "publish" && arg == "9") { "unsupported bridge protocol" }
        val helper = extras?.getBinder("helper") ?: error("missing helper binder")
        helper.linkToDeath({
            if (BridgeBinderRegistry.helper === helper) BridgeBinderRegistry.helper = null
        }, 0)
        BridgeBinderRegistry.helper = helper
        DiagnosticJournal.record("bridge-binder", "shell helper registered, protocol=9")
        return Bundle().apply { putBinder("owner", BridgeBinderRegistry.owner) }
    }
    override fun query(uri: Uri, projection: Array<out String>?, selection: String?, selectionArgs: Array<out String>?, sortOrder: String?): Cursor? = null
    override fun getType(uri: Uri): String? = null
    override fun insert(uri: Uri, values: ContentValues?): Uri? = null
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?) = 0
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?) = 0
}
