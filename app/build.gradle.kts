plugins {
    id("com.android.application")
}

val gomobileBind by tasks.registering(Exec::class) {
    val goModule = rootProject.layout.projectDirectory.dir("datapath")
    val outputAar = layout.buildDirectory.file("gomobile/datapath.aar")
    inputs.dir(goModule)
    outputs.file(outputAar)
    workingDir = goModule.asFile

    val goBin = providers.environmentVariable("GOBIN")
        .orElse(providers.systemProperty("user.home").map { "$it/go/bin" })
    val sdkRoot = providers.environmentVariable("ANDROID_SDK_ROOT")
        .orElse(providers.environmentVariable("ANDROID_HOME"))
    environment("PATH", goBin.map { "$it:${System.getenv("PATH")}" }.get())
    environment("ANDROID_NDK_HOME", sdkRoot.map { "$it/ndk/28.2.13676358" }.get())
    environment("ANDROID_HOME", sdkRoot.get())

    doFirst {
        outputAar.get().asFile.parentFile.mkdirs()
    }
    commandLine(
        "${goBin.get()}/gomobile",
        "bind",
        "-target=android/arm64",
        "-androidapi", "30",
        "-o", outputAar.get().asFile.absolutePath,
        ".",
    )
}

android {
    namespace = "com.argus.connect"
    compileSdk = 36
    ndkVersion = "28.2.13676358"

    defaultConfig {
        applicationId = "com.argus.connect.alpha"
        minSdk = 30
        targetSdk = 36
        versionCode = 193
        versionName = "1.4.53-alpha.5.53"
        ndk { abiFilters += "arm64-v8a" }
    }

    buildTypes {
        debug {
            applicationIdSuffix = ".debug"
            versionNameSuffix = "-debug"
        }
        release {
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        buildConfig = true
    }

    packaging.resources.excludes += setOf(
        "META-INF/AL2.0",
        "META-INF/LGPL2.1",
    )
}

tasks.named("preBuild") { dependsOn(gomobileBind) }

dependencies {
    implementation(files(layout.buildDirectory.file("gomobile/datapath.aar")))
    implementation("androidx.core:core:1.17.0")
    implementation("com.flyfishxu:kadb:2.1.1")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.10.2")
    implementation("org.lsposed.hiddenapibypass:hiddenapibypass:6.1")
}
