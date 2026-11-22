@file:Suppress("ktlint")

package eu.kanade.tachiyomi

/** Usado por extensiones mediante reflexión (extension-lib 1.3+). */
object AppInfo {
    fun getVersionCode() = 1
    fun getVersionName() = "0.1.0"
    fun getSupportedImageMimeTypes(): List<String> =
        listOf("image/jpeg", "image/png", "image/gif", "image/webp", "image/avif")
}
