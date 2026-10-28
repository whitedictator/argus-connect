package com.argus.connect

internal object MediaSampleCheck {
    fun isVideo(bytes: ByteArray): Boolean {
        if (bytes.size < 16) return false
        val webm = bytes[0] == 0x1a.toByte() && bytes[1] == 0x45.toByte() && bytes[2] == 0xdf.toByte() && bytes[3] == 0xa3.toByte()
        val box = bytes.copyOfRange(4,8).toString(Charsets.US_ASCII)
        return webm || box == "ftyp" || box == "styp"
    }
}
