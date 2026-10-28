package com.argus.connect.bridge

import android.content.Context
import android.os.Looper
import org.lsposed.hiddenapibypass.HiddenApiBypass

internal object HiddenApiAccess {
    fun unseal() {
        check(HiddenApiBypass.addHiddenApiExemptions("")) {
            "could not lift hidden-API restrictions"
        }
    }

    fun shellContext(): Context {
        // app_process does not install a Looper before invoking our main().
        // ActivityThread.systemMain() constructs Handler-backed framework state,
        // so Samsung aborts the helper unless the current thread is prepared.
        if (Looper.myLooper() == null) Looper.prepareMainLooper()
        val threadClass = Class.forName("android.app.ActivityThread")
        val thread = threadClass.getMethod("systemMain").invoke(null)
        val system = threadClass.getMethod("getSystemContext").invoke(thread) as Context
        val shell = system.createPackageContext("com.android.shell", 0)

        runCatching {
            val field = shell.javaClass.getDeclaredField("mAttributionSource")
            field.isAccessible = true
            val source = field.get(shell)
            val rebased = source.javaClass
                .getMethod("withPackageName", String::class.java)
                .invoke(source, "com.android.shell")
            field.set(shell, rebased)
        }
        return shell
    }
}
