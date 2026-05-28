package main

// serviceIntrospectXML is the D-Bus introspection XML for the Service object.
const serviceIntrospectXML = `<!DOCTYPE node PUBLIC "-//freedesktop//DTD D-BUS Object Introspection 1.0//EN"
  "http://www.freedesktop.org/standards/dbus/1.0/introspect.dtd">
<node>
  <interface name="org.freedesktop.Secret.Service">
    <method name="OpenSession">
      <arg name="algorithm" type="s" direction="in"/>
      <arg name="input"     type="v" direction="in"/>
      <arg name="output"    type="v" direction="out"/>
      <arg name="result"    type="o" direction="out"/>
    </method>
    <method name="CreateCollection">
      <arg name="properties" type="a{sv}" direction="in"/>
      <arg name="alias"      type="s"     direction="in"/>
      <arg name="collection" type="o"     direction="out"/>
      <arg name="prompt"     type="o"     direction="out"/>
    </method>
    <method name="SearchItems">
      <arg name="attributes" type="a{ss}" direction="in"/>
      <arg name="unlocked"   type="ao"    direction="out"/>
      <arg name="locked"     type="ao"    direction="out"/>
    </method>
    <method name="Unlock">
      <arg name="objects"  type="ao" direction="in"/>
      <arg name="unlocked" type="ao" direction="out"/>
      <arg name="prompt"   type="o"  direction="out"/>
    </method>
    <method name="Lock">
      <arg name="objects" type="ao" direction="in"/>
      <arg name="locked"  type="ao" direction="out"/>
      <arg name="prompt"  type="o"  direction="out"/>
    </method>
    <method name="GetSecrets">
      <arg name="items"   type="ao"          direction="in"/>
      <arg name="session" type="o"           direction="in"/>
      <arg name="secrets" type="a{o(oayays)}" direction="out"/>
    </method>
    <method name="SetAlias">
      <arg name="name"       type="s" direction="in"/>
      <arg name="collection" type="o" direction="in"/>
    </method>
    <property name="Collections" type="ao" access="read"/>
  </interface>
  <interface name="org.freedesktop.DBus.Properties">
    <method name="Get">
      <arg name="interface_name" type="s" direction="in"/>
      <arg name="property_name"  type="s" direction="in"/>
      <arg name="value"          type="v" direction="out"/>
    </method>
    <method name="GetAll">
      <arg name="interface_name" type="s"    direction="in"/>
      <arg name="props"          type="a{sv}" direction="out"/>
    </method>
    <method name="Set">
      <arg name="interface_name" type="s" direction="in"/>
      <arg name="property_name"  type="s" direction="in"/>
      <arg name="value"          type="v" direction="in"/>
    </method>
  </interface>
  <interface name="org.freedesktop.DBus.Introspectable">
    <method name="Introspect">
      <arg name="xml_data" type="s" direction="out"/>
    </method>
  </interface>
</node>`

// collectionIntrospectXML is the D-Bus introspection XML for the Collection object.
const collectionIntrospectXML = `<!DOCTYPE node PUBLIC "-//freedesktop//DTD D-BUS Object Introspection 1.0//EN"
  "http://www.freedesktop.org/standards/dbus/1.0/introspect.dtd">
<node>
  <interface name="org.freedesktop.Secret.Collection">
    <method name="Delete">
      <arg name="prompt" type="o" direction="out"/>
    </method>
    <method name="SearchItems">
      <arg name="attributes" type="a{ss}" direction="in"/>
      <arg name="results"    type="ao"    direction="out"/>
    </method>
    <method name="CreateItem">
      <arg name="properties" type="a{sv}"    direction="in"/>
      <arg name="secret"     type="(oayays)" direction="in"/>
      <arg name="replace"    type="b"        direction="in"/>
      <arg name="item"       type="o"        direction="out"/>
      <arg name="prompt"     type="o"        direction="out"/>
    </method>
    <property name="Items"    type="ao" access="read"/>
    <property name="Label"    type="s"  access="readwrite"/>
    <property name="Locked"   type="b"  access="read"/>
    <property name="Created"  type="t"  access="read"/>
    <property name="Modified" type="t"  access="read"/>
  </interface>
  <interface name="org.freedesktop.DBus.Properties">
    <method name="Get">
      <arg name="interface_name" type="s" direction="in"/>
      <arg name="property_name"  type="s" direction="in"/>
      <arg name="value"          type="v" direction="out"/>
    </method>
    <method name="GetAll">
      <arg name="interface_name" type="s"    direction="in"/>
      <arg name="props"          type="a{sv}" direction="out"/>
    </method>
    <method name="Set">
      <arg name="interface_name" type="s" direction="in"/>
      <arg name="property_name"  type="s" direction="in"/>
      <arg name="value"          type="v" direction="in"/>
    </method>
  </interface>
  <interface name="org.freedesktop.DBus.Introspectable">
    <method name="Introspect">
      <arg name="xml_data" type="s" direction="out"/>
    </method>
  </interface>
</node>`

// itemIntrospectXML is the D-Bus introspection XML for an Item object.
const itemIntrospectXML = `<!DOCTYPE node PUBLIC "-//freedesktop//DTD D-BUS Object Introspection 1.0//EN"
  "http://www.freedesktop.org/standards/dbus/1.0/introspect.dtd">
<node>
  <interface name="org.freedesktop.Secret.Item">
    <method name="Delete">
      <arg name="prompt" type="o" direction="out"/>
    </method>
    <method name="GetSecret">
      <arg name="session" type="o"        direction="in"/>
      <arg name="secret"  type="(oayays)" direction="out"/>
    </method>
    <method name="SetSecret">
      <arg name="secret" type="(oayays)" direction="in"/>
    </method>
    <property name="Locked"     type="b"    access="read"/>
    <property name="Attributes" type="a{ss}" access="readwrite"/>
    <property name="Label"      type="s"    access="readwrite"/>
    <property name="Created"    type="t"    access="read"/>
    <property name="Modified"   type="t"    access="read"/>
  </interface>
  <interface name="org.freedesktop.DBus.Properties">
    <method name="Get">
      <arg name="interface_name" type="s" direction="in"/>
      <arg name="property_name"  type="s" direction="in"/>
      <arg name="value"          type="v" direction="out"/>
    </method>
    <method name="GetAll">
      <arg name="interface_name" type="s"    direction="in"/>
      <arg name="props"          type="a{sv}" direction="out"/>
    </method>
    <method name="Set">
      <arg name="interface_name" type="s" direction="in"/>
      <arg name="property_name"  type="s" direction="in"/>
      <arg name="value"          type="v" direction="in"/>
    </method>
  </interface>
  <interface name="org.freedesktop.DBus.Introspectable">
    <method name="Introspect">
      <arg name="xml_data" type="s" direction="out"/>
    </method>
  </interface>
</node>`
