package com.argus.connect

enum class BridgePhase {
    SETUP_REQUIRED,
    READY,
    STARTING,
    ACTIVE,
    STOPPING,
    ERROR,
}

data class BridgeState(
    val phase: BridgePhase = BridgePhase.SETUP_REQUIRED,
    val title: String = "Требуется первоначальная настройка",
    val detail: String = "Argus подключится к системному bridge один раз",
    val vpnName: String? = null,
    val interfaceName: String? = null,
) {
    val isBusy: Boolean get() = phase == BridgePhase.STARTING || phase == BridgePhase.STOPPING
    val isActive: Boolean get() = phase == BridgePhase.ACTIVE
}
