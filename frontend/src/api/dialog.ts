/**
 * 原生文件选择对话框。
 *
 * 直接用 Wails v3 运行时的 Dialogs 模块：对话框由框架内置的 dialog 服务实现，
 * 因此【不需要】在 Go 侧注册服务、也不需要重新生成绑定 ——
 * 少一层绑定就少一处随 Wails beta 版本漂移的地方。
 *
 * 非 Wails 宿主（`npm run dev` 单独调样式、CI 构建）里调用会抛错，
 * 这时返回 available=false，由调用方降级为「手动输入路径」。
 */
import { Dialogs } from '@wailsio/runtime'

export interface PickOutcome {
  /** 是否选到了文件（用户点「取消」时为 false，但 available 仍为 true）。 */
  picked: boolean
  paths: string[]
  /** 原生对话框在此环境是否可用。false ⇒ 调用方应给出替代输入方式。 */
  available: boolean
}

/**
 * 弹出原生文件选择框。
 *
 * @param multiple 是否允许多选。v1.0 的后端 TransferService.SendFile 一次只接受
 *                 一个路径，因此默认单选；多选由调用方自行串行发起。
 */
export async function pickFiles(multiple = false): Promise<PickOutcome> {
  try {
    // 重载解析：AllowsMultipleSelection 为 true 时返回 string[]，否则返回 string
    const res = await Dialogs.OpenFile({
      Title: multiple ? '选择要发送的文件（可多选）' : '选择要发送的文件',
      CanChooseFiles: true,
      CanChooseDirectories: false,
      AllowsMultipleSelection: multiple,
      ButtonText: '发送'
    })
    const paths = (Array.isArray(res) ? res : [res]).filter((p): p is string => !!p)
    return { picked: paths.length > 0, paths, available: true }
  } catch {
    return { picked: false, paths: [], available: false }
  }
}

/**
 * 只选图片文件。
 *
 * 对话框层面过滤只是为了让用户少选错一次；真实格式判定仍在 Go 侧按魔数做，
 * 因为用户随时可以选一个「扩展名叫 .png 的文本」。
 */
export async function pickImages(): Promise<PickOutcome> {
  try {
    const res = await Dialogs.OpenFile({
      Title: '选择要发送的图片',
      CanChooseFiles: true,
      CanChooseDirectories: false,
      AllowsMultipleSelection: true,
      Filters: [{ DisplayName: '图片', Pattern: '*.png;*.jpg;*.jpeg;*.gif;*.webp;*.bmp' }],
      ButtonText: '发送'
    })
    const paths = (Array.isArray(res) ? res : [res]).filter((p): p is string => !!p)
    return { picked: paths.length > 0, paths, available: true }
  } catch {
    return { picked: false, paths: [], available: false }
  }
}

/**
 * 原生保存对话框，返回用户选择的完整路径（取消或不可用时为空串）。
 *
 * 与 pickFiles 同理：直接用 Wails 运行时，不在 Go 侧再注册一个服务。
 */
export async function saveFile(defaultName: string): Promise<string> {
  try {
    const res = await Dialogs.SaveFile({
      Title: '保存图片',
      Filename: defaultName,
      CanCreateDirectories: true,
      Filters: [{ DisplayName: '图片', Pattern: '*.jpg;*.jpeg;*.png;*.gif' }],
      ButtonText: '保存'
    })
    return typeof res === 'string' ? res : ''
  } catch {
    return ''
  }
}
