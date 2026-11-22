// rensaigo: host fino de extensiones.
// AndroidCompat y la API de sources vienen del código de Suwayomi-Server fijado en vendor/suwayomi
// (tag v2.4.2366, commit 40bc5da57820324ed0ae724cbbc020cc4673b594; ver vendor.sh).
rootProject.name = "rensaigo-host"

dependencyResolutionManagement {
    versionCatalogs {
        create("libs") { from(files("vendor/suwayomi/gradle/libs.versions.toml")) }
    }
}

include(":AndroidCompat")
include(":AndroidCompat:Config")
project(":AndroidCompat").projectDir = file("vendor/suwayomi/AndroidCompat")
project(":AndroidCompat:Config").projectDir = file("vendor/suwayomi/AndroidCompat/Config")

enableFeaturePreview("TYPESAFE_PROJECT_ACCESSORS")
