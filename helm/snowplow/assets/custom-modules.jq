def update_string_schema_defaults($jsonSchemaString; $jsonValues; $enumMap):

  # 1) ENUM injection per section / property (works for nested paths like deployments[].features[])
  ( $jsonSchemaString | tostring ) as $schema0
  | (reduce ($enumMap | keys[]) as $section (
      $schema0;
      . as $s0
      | if ($s0 | test("(?s)\"" + $section + "\"\\s*:\\s*\\{")) then
          (
            $s0
            | capture(
                "(?s)"
                + "(?<before>.*\"" + $section + "\"\\s*:\\s*\\{)"
                + "(?<body>[\\s\\S]*?\"title\"\\s*:\\s*\"" + $section + "\"[\\s\\S]*?\\})"
                + "(?<after>.*)"
              )
          ) as $cap
          | $cap.body as $body0
          | (
              reduce ($enumMap[$section] | keys[]) as $prop (
                $body0;
                . as $b
                | ($enumMap[$section][$prop]) as $vals
                | if ($vals | type) != "array" or ($vals | length) == 0 then
                    $b
                  else
                    ($vals | map("\""+.+"\"") | join(", ")) as $enumItems
                    | ("[" + $enumItems + "]") as $enumLiteral
                    | if ($b | test("(?s)\"" + $prop + "\"[\\s\\S]*\"enum\"")) then
                        # enum already present under this section → replace its array
                        ($b
                        | gsub(
                            "(?s)(?<prefix>\"" + $prop + "\"[\\s\\S]*\"enum\"\\s*:\\s*)\\[[^]]*\\]"
                            ;
                            "\(.prefix)\($enumLiteral)"
                          ))
                      else
                        # no enum yet → inject after "type": "string" in <section>...<prop>.items
                        ($b
                        | gsub(
                            "(?s)(?<prefix>\"" + $prop
                            + "\"[\\s\\S]*?\"items\"[\\s\\S]*?\"type\"\\s*:\\s*\"string\")"
                            ;
                            "\(.prefix),\n                \"enum\": \($enumLiteral)"
                          ))
                      end
                  end
              )
            ) as $body1
          | $cap.before + $body1 + $cap.after
        else
          $s0
        end
    )
  ) as $schemaAfterEnums

  # 2) ARRAY-LEVEL DEFAULTS (arrays of objects)
  | (reduce ($jsonValues | paths(arrays)) as $p (
        $schemaAfterEnums;
        . as $s
        | ($jsonValues | getpath($p)) as $arr
        | if (($arr | length) == 0)
             or (any($arr[]; type != "object"))
          then
            $s
          else
            ($arr | tojson) as $arrLiteral
            | ($p | map(select(type=="string"))) as $keys
            | if ($keys | length) == 0 then
                $s
              else
                ($keys[0:-1]) as $anc
                | ($keys | last) as $last
                | ($anc
                   | map("\""+.+"\"\\s*:\\s*\\{[\\s\\S]*?")
                   | join("")) as $chain
                | (
                    "(?s)"
                    + "(?<before>.*?"
                    +  $chain
                    + "\"" + $last
                    + "\"\\s*:\\s*\\{[\\s\\S]*?\"items\"\\s*:\\s*\\{[\\s\\S]*?\"type\"\\s*:\\s*\"object\"[\\s\\S]*?\\})"
                    + "(?<sep>,\\s*\"title\"\\s*:\\s*\"[^\"]*\"[\\s\\S]*?\\})"
                    + "(?<after>.*)"
                  ) as $re
                | if ($s | type) != "string" then
                    $s
                  elif ($s | test($re)) then
                    ($s | capture($re))
                    | (.before + ", \"default\": " + $arrLiteral + .sep + .after)
                  else
                    $s
                  end
              end
          end
    )
  ) as $afterArrays

  # 3) SCALAR DEFAULTS (non-object types, not inside array indices)
  | (reduce (
        $jsonValues
        | paths(scalars)
        | select(all(.[]; type=="string"))
    ) as $p (
        $afterArrays;
        . as $s
        | ($jsonValues | getpath($p)) as $val
        | ($val | tojson) as $lit
        | ($p | map(select(type=="string"))) as $keys
        | if ($keys | length) == 0 then
            $s
          else
            ($keys[0:-1]) as $anc
            | ($keys | last) as $last
            | ($anc
               | map("\""+.+"\"\\s*:\\s*\\{[\\s\\S]*?")
               | join("")) as $chain
            | (
                "(?s)"
                + "(?<before>.*?"
                +  $chain
                + "\"" + $last
                + "\"\\s*:\\s*\\{[\\s\\S]*?\"type\"\\s*:\\s*\"(string|number|integer|boolean|null)\"[\\s\\S]*?\"default\"\\s*:\\s*)"
                + "(\"[^\"]*\"|-?[0-9]+(?:\\.[0-9]+)?|true|false|null)"
                + "(?<after>[\\s\\S]*?\\})"
              ) as $re
            | if ($s | type) != "string" then
                $s
              elif ($s | test($re)) then
                ($s | capture($re))
                | (.before + $lit + .after)
              else
                $s
              end
          end
    )
  ) as $finalString

  | $finalString
;


def update_schema_defaults($input):
  $input as $root
  | ($root.getOrderedSchema) as $schema
  | reduce (
      $root.getCompositionSpec
      | paths(scalars)
    ) as $p
    (
      $schema;
      ($root.getCompositionSpec | getpath($p)) as $newValue
      | (
          ([$p[] | ["properties", .]] | add)
          + ["default"]
        ) as $schemaPath
      | setpath($schemaPath; $newValue)
    )
  | del(.properties.composition)
;