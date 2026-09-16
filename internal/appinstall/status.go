package appinstall

func installerStatus(step string) string {
	switch step {
	case "waiting_for_shutdown":
		return "Cerrando Workass…"
	case "checking_paths":
		return "Comprobando archivos de la aplicación…"
	case "waiting_for_files":
		return "Esperando que Windows libere los archivos…"
	case "replacing_files":
		return "Reemplazando archivos de la aplicación…"
	case "extracting_zip":
		return "Instalando la actualización…"
	case "launching":
		return "Abriendo Workass…"
	default:
		return "Actualizando Workass…"
	}
}
