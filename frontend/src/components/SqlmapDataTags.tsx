import { Tag } from 'antd'

type SqlmapDataTagSource = {
  has_db_names?: boolean
  has_table_names?: boolean
  has_column_names?: boolean
  has_row_data?: boolean
}

interface Props {
  item: SqlmapDataTagSource
  compact?: boolean
}

export default function SqlmapDataTags({ item, compact = false }: Props) {
  const style = compact ? { fontSize: 10, margin: 1 } : undefined

  return (
    <>
      {item.has_db_names && <Tag color="cyan" style={style}>库名</Tag>}
      {item.has_table_names && <Tag color="geekblue" style={style}>表名</Tag>}
      {item.has_column_names && <Tag color="purple" style={style}>列名</Tag>}
      {item.has_row_data && <Tag color="blue" style={style}>数据</Tag>}
    </>
  )
}
