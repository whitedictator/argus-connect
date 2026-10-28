package com.argus.connect

enum class LightPhase {
    OFF,
    ANALYZING,
    BASELINE,
    STARTING,
    VERIFYING,
    ACTIVE,
    LIMITED,
    STOPPING,
    ERROR,
}

enum class CheckResult { WAITING, ROUTE, WORKS, FALLBACK, FAILED }

data class ServiceCheck(
    val messages: CheckResult = CheckResult.WAITING,
    val calls: CheckResult = CheckResult.WAITING,
)

data class SocialCheck(
    val page: CheckResult = CheckResult.WAITING,
    val media: CheckResult = CheckResult.WAITING,
)

data class LightState(
    val phase: LightPhase = LightPhase.OFF,
    val title: String = "LIGHT выключен",
    val detail: String = "Российский IP сохраняется",
    val telegram: ServiceCheck = ServiceCheck(),
    val whatsapp: ServiceCheck = ServiceCheck(),
    val whatsappMedia: CheckResult = CheckResult.WAITING,
    val signal: ServiceCheck = ServiceCheck(),
    val threema: ServiceCheck = ServiceCheck(),
    val youtube: CheckResult = CheckResult.WAITING,
    val youtubeFeed: CheckResult = CheckResult.WAITING,
    val youtubeThumbnails: CheckResult = CheckResult.WAITING,
    val youtubeVideo: CheckResult = CheckResult.WAITING,
    val facebook: SocialCheck = SocialCheck(),
    val instagram: SocialCheck = SocialCheck(),
    val twitter: SocialCheck = SocialCheck(),
    val automaticChecksComplete: Boolean = false,
    val dohReady: Boolean = false,
    val dnsTransport: String = "CHECK",
    val strategySummary: String = "",
    val probeAttempts: Map<String, Int> = emptyMap(),
    val recoveringServices: Set<String> = emptySet(),
    val currentStrategies: Map<String, String> = emptyMap(),
    val currentRoutes: Map<String, String> = emptyMap(),
    val throughputKbps: Map<String, Long> = emptyMap(),
    val failureReasons: Map<String, String> = emptyMap(),
) {
    val isRunning: Boolean
        get() = phase == LightPhase.VERIFYING || phase == LightPhase.ACTIVE || phase == LightPhase.LIMITED
    val isBusy: Boolean
        get() = phase == LightPhase.ANALYZING || phase == LightPhase.STARTING || phase == LightPhase.STOPPING
}
